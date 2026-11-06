package main

// startup_shutdown_test.go — rework 1 finding 1/2 的守护测试：
//
//  1. 退出码确定性：自检失败与 SIGTERM 同时就绪时，裸 select 随机选分支，
//     选到 ctx.Done() 会让进程以 0 退出、失败被吞。修复后的编排（select 之后的
//     非阻塞 tie-break）必须**每一次**都走失败分支。循环复现 + 断言。
//  2. 走完整关停路径：诊断以 Error（而非 Fatal）记录，exit code 由 run() 返回、
//     由 main 的 os.Exit 执行——defer 与 shutdown 代码都在返回路径上。
//
// 变异自验：把编排恢复成「goroutine 先送 channel 再 stop()、main 裸 select」的
// 写法时，TestAwaitShutdownOrSelfCheckFailure_DeterministicUnderRace 必须红
// （见 report-rework1.md 的变异记录）。

import (
	"context"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/config"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/startupcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func testCfg() *config.Config {
	return &config.Config{
		MinIOEndpoint:  "s3.test:9000",
		MinIOAccessKey: "TESTACCESSKEYID",
		MinIORoleARN:   "arn:aws:iam:::role/agent-role",
	}
}

// 确定性（核心）：channel 已有失败 + ctx 同时取消 —— 每次都必须返回非零。
// 这是 rework 1 finding 1 的直接复现：裸 select 在两分支同时就绪时随机选择，
// 循环足够多次必然选中 ctx.Done()，旧写法在此稳定翻车。
func TestAwaitShutdownOrSelfCheckFailure_DeterministicUnderRace(t *testing.T) {
	logger := zap.NewNop()
	cfg := testCfg()

	const rounds = 200
	for i := 0; i < rounds; i++ {
		// 先取消 ctx，再投递失败：与生产时序（stop() 与 channel send 竞争）
		// 等价——两个分支在进入 select 时都已是就绪状态。
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ch := make(chan *startupcheck.Failure, 1)
		ch <- &startupcheck.Failure{Item: "s3-identity", Class: startupcheck.ClassAuthConfig}

		code := awaitShutdownOrSelfCheckFailure(ctx, ch, logger, cfg)
		require.NotZerof(t, code, "round %d: 自检失败与信号同时就绪时退出码为 0 —— 失败被吞（rework 1 finding 1 回归）", i)
	}
}

// 只有自检失败（无信号）：非零退出，且 channel 被消费（幂等，无泄漏等待）。
func TestAwaitShutdownOrSelfCheckFailure_FailureOnly(t *testing.T) {
	logger := zap.NewNop()
	cfg := testCfg()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan *startupcheck.Failure, 1)
	ch <- &startupcheck.Failure{Item: "sts-assume-role", Class: startupcheck.ClassAuthConfig}

	code := awaitShutdownOrSelfCheckFailure(ctx, ch, logger, cfg)
	assert.Equal(t, 1, code)
	// tie-break 已把失败取走；再 drain 不应有残留。
	_, ok := drainStartupCheckFailure(ch)
	assert.False(t, ok)
}

// 只有信号（无失败）：零退出 —— 正常关停路径不受影响。
func TestAwaitShutdownOrSelfCheckFailure_SignalOnly(t *testing.T) {
	logger := zap.NewNop()
	cfg := testCfg()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan *startupcheck.Failure, 1)

	code := awaitShutdownOrSelfCheckFailure(ctx, ch, logger, cfg)
	assert.Zero(t, code)
}

// 失败晚于信号到达（真正的时间先后，不是同时）：信号路径先走，tie-break
// 时刻 channel 仍空 → 零退出。这不是缺陷：关停已经开始后迟到的失败没有
// 「再退出一次」的意义；tie-break 只为同时就绪的竞争兜底。
func TestAwaitShutdownOrSelfCheckFailure_LateFailureAfterSignal(t *testing.T) {
	logger := zap.NewNop()
	cfg := testCfg()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan *startupcheck.Failure, 1) // still empty

	code := awaitShutdownOrSelfCheckFailure(ctx, ch, logger, cfg)
	assert.Zero(t, code)
}

// 诊断以 Error 级别记录（不是 Fatal —— Fatal 会 os.Exit 跳过关停路径），
// 且包含全部 IC-BUG-40 诊断字段。
func TestAwaitShutdownOrSelfCheckFailure_LogsErrorNotFatal(t *testing.T) {
	core, obs := observer.New(zap.ErrorLevel)
	logger := zap.New(core)
	cfg := testCfg()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan *startupcheck.Failure, 1)
	ch <- &startupcheck.Failure{
		Item: "s3-identity", Class: startupcheck.ClassAuthConfig,
		Err: assert.AnError, Window: 15 * time.Second, Attempts: 5,
	}

	code := awaitShutdownOrSelfCheckFailure(ctx, ch, logger, cfg)
	assert.Equal(t, 1, code)

	entries := obs.FilterMessage("storage startup self-check failed — refusing to run with a broken storage identity (IC-BUG-40)")
	require.Equal(t, 1, entries.Len(), "诊断必须以 Error 记录一次（Fatal 会跳过关停路径）")
	entry := entries.All()[0]
	assert.Equal(t, zap.ErrorLevel, entry.Level)
	keys := map[string]string{}
	for _, f := range entry.Context {
		keys[f.Key] = f.String
	}
	for _, k := range []string{"item", "class", "endpoint", "access_key_id", "fix_hint"} {
		assert.NotEmpty(t, keys[k], "缺少诊断字段 %s", k)
	}
	assert.Equal(t, "s3.test:9000", keys["endpoint"])
	assert.Equal(t, "TESTACCESSKEYID", keys["access_key_id"])
	// 绝不打 secret：日志字段集合里根本没有 secret 字段。
	assert.NotContains(t, keys, "secret")
}

// drainStartupCheckFailure 的两分支：空 channel 返回 false，有待取失败返回 true。
func TestDrainStartupCheckFailure(t *testing.T) {
	empty := make(chan *startupcheck.Failure, 1)
	if _, ok := drainStartupCheckFailure(empty); ok {
		t.Fatal("drain(empty) = true, want false")
	}
	filled := make(chan *startupcheck.Failure, 1)
	want := &startupcheck.Failure{Item: "x"}
	filled <- want
	got, ok := drainStartupCheckFailure(filled)
	if !ok || got != want {
		t.Fatalf("drain(filled) = %v, %v; want %v, true", got, ok, want)
	}
}
