package main

// startup_exit_guard_test.go — rework 2 finding 1 的结构守护（AST 探针）：
//
// 返工 1 把 logger.Fatal 机械替换成 logger.Error 却漏了 return，导致「迁移失败
// 照常运行 / nil DB panic」的 fail-open。人肉数 12 处不可靠，本测试用 AST 钉住
// 两条不变式：
//
//  1. run() 体内不得出现 logger.Fatal / os.Exit（它们会跳过关停与 defer；退出
//     必须经 return code）。
//  2. run() 体内凡「if err != nil { logger.Error(...) }」形状的错误块，块内
//     最后一条语句必须是 return —— 否则就是「记日志后继续跑」的 fail-open。
//
// 变异自验（见 report-rework2.md）：删掉任一处 return 1 → 本测试红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// findRunFunc parses main.go and returns the run() FuncDecl.
func findRunFunc(t *testing.T) *ast.FuncDecl {
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
			return fn
		}
	}
	t.Fatal("main.go 缺少 run() 函数")
	return nil
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

// lastStmt 返回块的最后一条语句（空块返回 nil）。
func lastStmt(block *ast.BlockStmt) ast.Stmt {
	if len(block.List) == 0 {
		return nil
	}
	return block.List[len(block.List)-1]
}

// run() 体内不得有 logger.Fatal / os.Exit。
func TestRunBodyHasNoFatalOrOsExit(t *testing.T) {
	run := findRunFunc(t)
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

// 每个「if err != nil { logger.Error(...) }」错误块必须以 return 结尾。
//
// 形状匹配：if 语句的 Cond 含对某个错误的 != nil 比较（含 errors.Is 等调用形态
// 的退化情况——只要 if 体首句是 logger.Error 且体长>1 就要求末句 return）。
// 故意的例外（不匹配「启动失败必须终止」的语义）：
//   - 体内只有 logger.Error 一条语句且该 Error 属于运行期可恢复告警的调用点
//     ——这些点在白名单里逐个列出并写明理由。
func TestRunErrorBlocksEndWithReturn(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("main.go 缺少 run()")
	}

	// 可恢复的运行期告警（logger.Error 后继续跑是正确行为）——按消息前缀
	// 锚定到调用点；白名单必须逐条写明理由（见各 prefix 的注释）。
	recoverablePrefixes := []string{
		// Shutdown-path diagnostics (already exiting) — not startup guards.
		"HTTP server shutdown error",
		"storage event source adapter did not stop",
		"control plane stopped with",
		// Runtime-degraded (not startup-fatal): the dead-letter sink probe
		// logs degraded state and /healthz reflects it dynamically (S-1/S-2).
		"webhook dead-letter sink NOT ready",
	}
	recoverable := map[int]bool{} // line -> recoverable Error
	ast.Inspect(run, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok || !isLoggerErrorCall(ce) {
			return true
		}
		if len(ce.Args) > 0 {
			if lit, ok := ce.Args[0].(*ast.BasicLit); ok {
				for _, p := range recoverablePrefixes {
					if containsPrefix(lit.Value, p) {
						recoverable[fset.Position(ce.Pos()).Line] = true
					}
				}
			}
		}
		return true
	})

	goBodies := collectGoLiteralNodes(run)
	// Mark every if-statement nested inside a listener goroutine body.
	inGoLiteral := map[ast.Node]bool{}
	{
		tmp := map[ast.Node]bool{}
		ast.Inspect(run, func(n ast.Node) bool {
			if ifs, ok := n.(*ast.IfStmt); ok && nodeInsideAny(ifs, goBodies) {
				tmp[ifs] = true
			}
			return true
		})
		inGoLiteral = tmp
	}

	// Now the invariant: every if-block whose FIRST statement is a
	// logger.Error call (and which is not in the recoverable set) must be
	// TERMINAL — one of:
	//   a) ends with return; or
	//   b) contains a serveFatal send — the failure is handed to the shutdown
	//      orchestration, which converts it to the non-zero exit (used where
	//      listeners are already up and a bare return would strand them).
	//   c) sits inside a `go` func literal — a listener goroutine reporting
	//      its own death through serveFatal; there is no return path to take.
	bad := 0
	ast.Inspect(run, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Body == nil {
			return true
		}
		if len(ifs.Body.List) == 0 {
			return true
		}
		first, ok := ifs.Body.List[0].(*ast.ExprStmt)
		if !ok || !isLoggerErrorCall(first.X) {
			return true
		}
		line := fset.Position(first.Pos()).Line
		if recoverable[line] {
			return true
		}
		if _, isReturn := lastStmt(ifs.Body).(*ast.ReturnStmt); isReturn {
			return true
		}
		if blockSendsToServeFatal(ifs.Body) {
			return true
		}
		if inGoLiteral[ifs] {
			return true
		}
		bad++
		t.Errorf("main.go:%d — if 块以 logger.Error 开头且既不 return 也不送 serveFatal：记日志后继续运行（rework 2 finding 1 的 fail-open 形状）", line)
		return true
	})
	if bad > 0 {
		t.Fatalf("发现 %d 处「Error 后不终止」的错误块", bad)
	}
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

// collectGoLiteralNodes 收集 run() 体内所有 `go func(){...}` 语句的函数体
// 节点，用于判断某个 if 是否位于 listener goroutine 内（那里没有可走的
// return 路径，错误只能经 serveFatal 上报）。
func collectGoLiteralNodes(run *ast.FuncDecl) map[ast.Node]bool {
	set := map[ast.Node]bool{}
	ast.Inspect(run, func(n ast.Node) bool {
		gostmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		if fl, ok := gostmt.Call.Fun.(*ast.FuncLit); ok && fl.Body != nil {
			set[fl.Body] = true
		}
		return true
	})
	return set
}

// nodeInsideAny 报告 target 是否嵌套在 set 中任一节点的子树内。
func nodeInsideAny(target ast.Node, set map[ast.Node]bool) bool {
	for root := range set {
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

// containsPrefix 报告带引号的 Go 字符串字面量 value 是否包含 msg 前缀
// （字面量带双引号，消息本身无引号，直接子串判断即可）。
func containsPrefix(quotedLit, msg string) bool {
	return len(quotedLit) >= len(msg) && indexOf(quotedLit, msg) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
