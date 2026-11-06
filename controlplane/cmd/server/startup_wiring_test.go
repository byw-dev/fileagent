package main

// startup_wiring_test.go — IC-BUG-40 的接线守护（AST 结构测试，仿
// storage_event_source_test.go 的 N2 探针）：
//
//	main.go 必须真正调用 startupcheck.Run（变异 a：删掉调用 → 红），
//	且 fatal 退出路径必须在 main 的 select 里消费 selfCheckFailed
//	（变异 b 的接线面：窗口用完不退出 → select 分支缺失即红）。
//
// 行为面（变异 b/c 的语义部分）由 startupcheck 包的单测守护：
//	- b「窗口用完后不退出」→ TestRun_S3AuthFailure_RetriesThenFailsAtWindowEnd
//	  / TestRun_NetworkError_WindowExhausted 断言 Run 在窗口耗尽时返回 *Failure；
//	- c「跳过 STS」→ TestRun_STSFailure 断言 STS 失败会让整个自检失败
//	  （若有人删掉 STS 探测，fakeSTS 的调用计数与失败传播都会消失）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// main 必须调用 startupcheck.Run —— 「不调用自检」的变异在这里变红。
func TestMainCallsStartupCheckRun(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	called := false
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := ce.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "startupcheck" && sel.Sel.Name == "Run" {
				called = true
			}
		}
		return true
	})
	if !called {
		t.Fatal("IC-BUG-40 回归：main.go 不再调用 startupcheck.Run —— CP 回到「凭据配错照常启动」的缺陷状态")
	}
}

// main 的 select 必须消费 selfCheckFailed 并走 Fatal —— 「窗口用完后
// 不退出」的变异（删掉 select 分支）在这里变红。
func TestMainSelectsOnSelfCheckFailure(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	recvCase := false
	fatalOnFailure := false
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectStmt)
		if !ok {
			return true
		}
		for _, c := range sel.Body.List {
			cl, ok := c.(*ast.CommClause)
			if !ok {
				continue
			}
			if comm, ok := cl.Comm.(*ast.AssignStmt); ok {
				for _, rhs := range comm.Rhs {
					if op, ok := rhs.(*ast.UnaryExpr); ok && op.Op.String() == "<-" {
						if id, ok := op.X.(*ast.Ident); ok && id.Name == "selfCheckFailed" {
							recvCase = true
						}
					}
				}
			}
			// The fatal must reference the failure log path (LogFields on it).
			for _, stmt := range cl.Body {
				ast.Inspect(stmt, func(n ast.Node) bool {
					if ce, ok := n.(*ast.CallExpr); ok {
						if m, ok := ce.Fun.(*ast.SelectorExpr); ok {
							if id, ok := m.X.(*ast.Ident); ok && id.Name == "logger" && m.Sel.Name == "Fatal" {
								fatalOnFailure = true
							}
						}
					}
					return true
				})
			}
		}
		return true
	})
	if !recvCase {
		t.Fatal("IC-BUG-40 回归：main 的 select 缺少 <-selfCheckFailed 分支 —— 自检窗口耗尽后进程不会退出")
	}
	if !fatalOnFailure {
		t.Fatal("IC-BUG-40 回归：selfCheckFailed 分支必须以 logger.Fatal 结束（非零退出 + 运维可 grep 的级别）")
	}
}
