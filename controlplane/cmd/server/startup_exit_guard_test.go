package main

// startup_exit_guard_test.go — run() 退出语义的结构守护（AST 探针）。
//
// 返工 1 曾把 logger.Fatal 机械替换成 logger.Error 却漏了 return（「记日志
// 后继续跑」的 fail-open）；返工 3（评审第 6 条）再把守护收紧：
//
//  1. run() 体内不得出现 logger.Fatal / os.Exit（退出必须经 return code）。
//  2. 若某 if 块以 logger.Error 开头，它必须处于以下三类之一：
//     a) 以 return 结尾（启动失败终止进程）；
//     b) 位于 awaitShutdownOrSelfCheckFailure 调用**之后**（关停阶段的诊断，
//        此时进程已在退出路上，Error 后继续执行关停步骤是正确行为）；
//     c) 位于 go 语句的函数字面量内（listener goroutine）**且块内真的投递
//        serveFatal** —— 不再整体豁免 goroutine：漏投递的 listener 死亡会
//        让进程挂着（返工 2 finding 2 的形状）。
//
// 豁免不再按「消息子串」判断（评审意见：改个文案就能绕过），只按 AST 位置
// 与控制流分类。可恢复的运行期告警（如 dead-letter sink 降级）不在 if 块
// 首句为 logger.Error 的形状里（probeSink 用的是 switch），天然不受本探针
// 约束；若未来有人把它改成 if 形状，本测试会要求给出 return 或移到关停
// 阶段——那正是应当被审视的改动。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// findRunFunc parses main.go and returns the run() FuncDecl.
func findRunFunc(t *testing.T) (*ast.FuncDecl, *token.FileSet) {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
			return fn, fset
		}
	}
	t.Fatal("main.go 缺少 run() 函数")
	return nil, nil
}

// isLoggerErrorCall 报告 expr 是否为 logger.Error(...) 调用。
func isLoggerErrorCall(expr ast.Expr) bool {
	ce, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "logger" && sel.Sel.Name == "Error"
}

// blockSendsToServeFatal 报告块内是否存在 serveFatal <- ... 发送语句。
func blockSendsToServeFatal(block *ast.BlockStmt) bool {
	found := false
	ast.Inspect(block, func(n ast.Node) bool {
		if send, ok := n.(*ast.SendStmt); ok {
			if id, ok := send.Chan.(*ast.Ident); ok && id.Name == "serveFatal" {
				found = true
			}
		}
		return true
	})
	return found
}

// orchestrationCallLine 返回 run() 体内 awaitShutdownOrSelfCheckFailure(...)
// 调用的行号（0 = 不存在，由 wiring 测试负责报告）。
func orchestrationCallLine(run *ast.FuncDecl, fset *token.FileSet) int {
	line := 0
	ast.Inspect(run, func(n ast.Node) bool {
		if ce, ok := n.(*ast.CallExpr); ok {
			if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "awaitShutdownOrSelfCheckFailure" {
				line = fset.Position(ce.Pos()).Line
			}
		}
		return true
	})
	return line
}

// goLiteralBodies 收集 run() 体内所有 `go func(){...}` 的函数体。
func goLiteralBodies(run *ast.FuncDecl) []ast.Node {
	var bodies []ast.Node
	ast.Inspect(run, func(n ast.Node) bool {
		if gostmt, ok := n.(*ast.GoStmt); ok {
			if fl, ok := gostmt.Call.Fun.(*ast.FuncLit); ok && fl.Body != nil {
				bodies = append(bodies, fl.Body)
			}
		}
		return true
	})
	return bodies
}

// nodeInsideAny 报告 target 是否嵌套在 candidates 中任一节点的子树内。
func nodeInsideAny(target ast.Node, candidates []ast.Node) bool {
	for _, root := range candidates {
		found := false
		ast.Inspect(root, func(n ast.Node) bool {
			if n == target {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// run() 体内不得有 logger.Fatal / os.Exit。
func TestRunBodyHasNoFatalOrOsExit(t *testing.T) {
	run, _ := findRunFunc(t)
	ast.Inspect(run, func(n ast.Node) bool {
		if ce, ok := n.(*ast.CallExpr); ok {
			if sel, ok := ce.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					if id.Name == "logger" && sel.Sel.Name == "Fatal" {
						t.Errorf("run() 体内出现 logger.Fatal（会 os.Exit 跳过关停与 defer；退出必须经 return code）")
					}
					if id.Name == "os" && sel.Sel.Name == "Exit" {
						t.Errorf("run() 体内出现 os.Exit（跳过 defer；退出必须经 return code，由 main 收尾）")
					}
				}
			}
		}
		return true
	})
}

// 每个「if 块以 logger.Error 开头」的错误块必须按 a/b/c 三类之一终止
// （见文件头注释）。豁免按 AST 位置分类，不按消息内容。
func TestRunErrorBlocksAreTerminal(t *testing.T) {
	run, fset := findRunFunc(t)
	orchLine := orchestrationCallLine(run, fset)
	if orchLine == 0 {
		t.Fatal("找不到 awaitShutdownOrSelfCheckFailure 调用（由 wiring 测试负责），无法划分关停阶段")
	}
	goBodies := goLiteralBodies(run)

	bad := 0
	ast.Inspect(run, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Body == nil || len(ifs.Body.List) == 0 {
			return true
		}
		first, ok := ifs.Body.List[0].(*ast.ExprStmt)
		if !ok || !isLoggerErrorCall(first.X) {
			return true
		}
		line := fset.Position(ifs.Pos()).Line

		// (a) ends with return.
		if _, isReturn := ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt); isReturn {
			return true
		}
		// (b) shutdown phase: AFTER the orchestration call.
		if line > orchLine {
			return true
		}
		// (c) inside a listener goroutine AND actually delivers serveFatal.
		if nodeInsideAny(ifs, goBodies) {
			if blockSendsToServeFatal(ifs.Body) {
				return true
			}
			bad++
			t.Errorf("main.go:%d — listener goroutine 内的错误块没有投递 serveFatal：listener 死亡会让进程挂着不退出（返工 2 finding 2 形状）", line)
			return true
		}
		// Also accept a non-goroutine block that hands the failure to the
		// shutdown orchestration via serveFatal (the adapter-init site).
		if blockSendsToServeFatal(ifs.Body) {
			return true
		}
		bad++
		t.Errorf("main.go:%d — if 块以 logger.Error 开头但不终止（既不 return，也不在关停阶段，也没投递 serveFatal）：记日志后继续运行（fail-open 形状）", line)
		return true
	})
	if bad > 0 {
		t.Fatalf("发现 %d 处不终止的错误块", bad)
	}
}

// 反向守护：goroutine 内**没有** serveFatal 投递的错误块必须被上面那条测试
// 抓红——本测试用一个人工构造的最小 run 形状验证探针本身的能力（变异
// 证据见 report-rework3.md：在真实 main.go 的 listener goroutine 里删掉
// serveFatal <- 后 TestRunErrorBlocksAreTerminal 变红）。
func TestGuardDetectsMissingServeFatalInGoroutine(t *testing.T) {
	src := `package main

func run() int {
	logger.Error("boot")
	serveFatal := make(chan error, 2)
	go func() {
		if err := serve(); err != nil {
			logger.Error("listener died")
			// no serveFatal send — must be flagged
		}
	}()
	return 0
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "probe.go", src, 0)
	if err != nil {
		t.Fatalf("parse probe: %v", err)
	}
	var run *ast.FuncDecl
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("probe 缺少 run()")
	}
	orchLine := orchestrationCallLine(run, fset)
	goBodies := goLiteralBodies(run)
	if len(goBodies) != 1 {
		t.Fatalf("probe 应有 1 个 go literal，得到 %d", len(goBodies))
	}
	flagged := false
	ast.Inspect(run, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Body == nil || len(ifs.Body.List) == 0 {
			return true
		}
		first, ok := ifs.Body.List[0].(*ast.ExprStmt)
		if !ok || !isLoggerErrorCall(first.X) {
			return true
		}
		line := fset.Position(ifs.Pos()).Line
		if _, isReturn := ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt); isReturn {
			return true
		}
		if orchLine != 0 && line > orchLine {
			return true
		}
		if nodeInsideAny(ifs, goBodies) && blockSendsToServeFatal(ifs.Body) {
			return true
		}
		if blockSendsToServeFatal(ifs.Body) {
			return true
		}
		flagged = true
		return true
	})
	if !flagged {
		t.Fatal("探针未能识别 goroutine 内漏投递 serveFatal 的错误块 —— 守护本身失效")
	}
}
