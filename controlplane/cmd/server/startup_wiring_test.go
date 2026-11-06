package main

// startup_wiring_test.go — IC-BUG-40 的接线守护（AST 结构测试，仿
// storage_event_source_test.go 的 N2 探针）：
//
//	main.go 必须真正调用 startupcheck.Run（变异 a：删掉调用 → 红），
//	退出路径必须在 select 里消费 selfCheckFailed 且**不得**用 logger.Fatal
//	（rework 1 finding 2：Fatal 会 os.Exit 跳过关停路径与 defer；正确契约是
//	Error 记录 + run() 返回非零码 + main 的 os.Exit 收尾）。

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

	// The orchestration now lives in awaitShutdownOrSelfCheckFailure; assert
	// (1) run() hands selfCheckFailed to it, and (2) the run() BODY (the
	// synchronous boot+shutdown path) contains no logger.Fatal — Fatal would
	// os.Exit(1) from inside run() and skip httpSrv.Shutdown, the event-source
	// join and every defer (rework 1 finding 2). The two listener goroutines
	// (gRPC/HTTP bind failures) are outside run()'s synchronous path and keep
	// Fatal deliberately: a dead listener means the process cannot continue.
	recvCase := false
	fatalInRunBody := false
	var runFunc *ast.FuncDecl
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
			runFunc = fn
		}
	}
	if runFunc == nil {
		t.Fatal("main.go 缺少 run() 函数（rework 1：退出码必须经 run() 返回、由 main 的 os.Exit 执行，defer 才有机会运行）")
	}
	ast.Inspect(runFunc, func(n ast.Node) bool {
		if ce, ok := n.(*ast.CallExpr); ok {
			if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "awaitShutdownOrSelfCheckFailure" {
				for _, arg := range ce.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == "selfCheckFailed" {
						recvCase = true
					}
				}
			}
			if m, ok := ce.Fun.(*ast.SelectorExpr); ok {
				if id, ok := m.X.(*ast.Ident); ok && id.Name == "logger" && m.Sel.Name == "Fatal" {
					fatalInRunBody = true
				}
			}
		}
		return true
	})
	if !recvCase {
		t.Fatal("IC-BUG-40 回归：run() 不再把 selfCheckFailed 交给 awaitShutdownOrSelfCheckFailure —— 自检窗口耗尽后进程不会以非零退出")
	}
	if fatalInRunBody {
		t.Fatal("rework 1 finding 2 回归：run() 体内仍有 logger.Fatal（Fatal 会 os.Exit 跳过 httpSrv.Shutdown / 事件源 join / 全部 defer；自检失败必须 Error + 非零返回码）")
	}
}
