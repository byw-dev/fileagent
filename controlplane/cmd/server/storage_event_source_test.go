package main

// storage_event_source_test.go — N2/N6 守护测试：
//   N6: newEventSink(true, …) 必须产出 suppression-aware sink（suppressed →
//       ErrSuppressed）；回退 plain sink 的变异（main/工厂改回 NewIndexSink）
//       会让本测试红。
//   N2: awaitEventSourceStop 的「等到退出」与「超时」两分支；外加 AST 结构
//       测试钉住 main.go 中「赋值给外层 adapterDone」（:= 遮蔽曾让 join 永不
//       执行——评审 r2-join.go 的探针正式化）。

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
)

// fakeSinkWriter 同时实现 IndexWriter 与 SuppressionReporting，suppressed 可配。
type fakeSinkWriter struct {
	suppressed bool
}

func (w *fakeSinkWriter) IndexUpload(context.Context, string, string, int64, string, time.Time, string, string) error {
	return nil
}
func (w *fakeSinkWriter) IndexDeletion(context.Context, string, string, time.Time, string, string) error {
	return nil
}
func (w *fakeSinkWriter) IndexUploadReporting(context.Context, string, string, int64, string, time.Time, string, string) (bool, error) {
	return w.suppressed, nil
}
func (w *fakeSinkWriter) IndexDeletionReporting(context.Context, string, string, time.Time, string, string) (bool, error) {
	return w.suppressed, nil
}
func (w *fakeSinkWriter) RowState(context.Context, string, string) (storageevent.RowState, error) {
	return storageevent.RowState{Found: true, Status: "completed", Size: 1, ETag: "same"}, nil
}

// N6：SeaweedFS 配置下 suppressed 写入必须产生 ErrSuppressed（aware sink 接线）。
func TestNewEventSink_SeaweedfsUsesAwareSink(t *testing.T) {
	sink := newEventSink(true, &fakeSinkWriter{suppressed: true})
	err := sink.Apply(context.Background(), storageevent.Event{
		Kind: storageevent.KindObjectCreated, Bucket: "b", Key: "k", Source: storageevent.SourceStorageReadback,
	})
	if !errors.Is(err, storageevent.ErrSuppressed) {
		t.Fatalf("SeaweedFS 接线的 sink = %v, want ErrSuppressed（回退 plain sink 的变异应在此变红）", err)
	}
}

// N6：MinIO 配置仍是 plain sink——suppressed 写入保持静默成功（行为不变）。
func TestNewEventSink_MinioKeepsPlainSink(t *testing.T) {
	sink := newEventSink(false, &fakeSinkWriter{suppressed: true})
	if err := sink.Apply(context.Background(), storageevent.Event{
		Kind: storageevent.KindObjectCreated, Bucket: "b", Key: "k", Source: storageevent.SourceMinioEvent,
	}); err != nil {
		t.Fatalf("MinIO 接线的 sink = %v, want nil（行为不变）", err)
	}
}

// N2 分支一：channel 关闭 → 等待成功返回 true。
func TestAwaitEventSourceStop_ReturnsWhenClosed(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if !awaitEventSourceStop(done, 50*time.Millisecond) {
		t.Fatal("awaitEventSourceStop(closed) = false, want true")
	}
}

// N2 分支二：channel 永不关闭 → 超时返回 false。
func TestAwaitEventSourceStop_TimesOut(t *testing.T) {
	done := make(chan struct{}) // never closed
	if awaitEventSourceStop(done, 50*time.Millisecond) {
		t.Fatal("awaitEventSourceStop(open) = true, want false (timeout)")
	}
}

// N2 分支零：done == nil（适配器未启用）→ 直接 true。
func TestAwaitEventSourceStop_NilChannel(t *testing.T) {
	if !awaitEventSourceStop(nil, 50*time.Millisecond) {
		t.Fatal("awaitEventSourceStop(nil) = false, want true")
	}
}

// N2 结构测试（评审 r2-join.go 探针正式化）：main.go 中
//
//	赋值 `adapterDone = done` 与 join `<-adapterDone`
//
// 必须绑定到同一个 adapterDone 声明。若有人把赋值改回 `adapterDone := …`
// （:= 遮蔽），join 引用的仍是外层 nil 声明 → 本测试红。
func TestAdapterDoneAssignmentAndJoinShareDeclaration(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// 遮蔽检测（精确、不依赖作用域解析）：main 内对 adapterDone 的所有赋值
	// 必须是 = （token.ASSIGN）；出现 := （token.DEFINE）即声明了新的局部变量
	// 遮蔽外层 —— join 引用外层声明，等待的是从未被关闭的 nil channel。
	defineLine := 0
	assignLine := 0
	joinLine := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch stmt := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range stmt.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "adapterDone" {
						if stmt.Tok == token.DEFINE {
							defineLine = fset.Position(stmt.Pos()).Line
						} else {
							assignLine = fset.Position(stmt.Pos()).Line
						}
					}
				}
			case *ast.CallExpr:
				for _, arg := range stmt.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == "adapterDone" {
						joinLine = fset.Position(stmt.Pos()).Line
					}
				}
			case *ast.UnaryExpr:
				if id, ok := stmt.X.(*ast.Ident); ok && id.Name == "adapterDone" && stmt.Op.String() == "<-" {
					joinLine = fset.Position(stmt.Pos()).Line
				}
			}
			return true
		})
	}

	if joinLine == 0 {
		t.Fatal("main.go 缺少 adapterDone 的 join（awaitEventSourceStop / <-adapterDone）")
	}
	if defineLine != 0 {
		t.Fatalf("N2 回归：line %d 用 := 给 adapterDone 赋值（遮蔽外层声明，退出 join 等待的是从未关闭的 nil channel）——必须用 = 赋值给外层变量", defineLine)
	}
	if assignLine == 0 {
		t.Fatalf("main.go 缺少对 adapterDone 的外层赋值（line=%d）", assignLine)
	}
}
