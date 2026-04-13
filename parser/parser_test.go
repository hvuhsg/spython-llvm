package parser

import (
	"testing"

	"github.com/yehoyadashtinmetz/spython/lexer"
)

func parseSource(t *testing.T, source string) *Program {
	t.Helper()
	l := lexer.New(source)
	tokens, err := l.Tokens()
	if err != nil {
		t.Fatalf("lexer error: %v", err)
	}
	p := New(tokens)
	prog, err := p.Parse()
	if err != nil {
		t.Fatalf("parser error: %v", err)
	}
	return prog
}

func TestParsePrintInt(t *testing.T) {
	prog := parseSource(t, "print(42)\n")
	if len(prog.Stmts) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(prog.Stmts))
	}
	exprStmt, ok := prog.Stmts[0].(*ExprStmt)
	if !ok {
		t.Fatalf("expected ExprStmt, got %T", prog.Stmts[0])
	}
	call, ok := exprStmt.Expr.(*CallExpr)
	if !ok {
		t.Fatalf("expected CallExpr, got %T", exprStmt.Expr)
	}
	ident, ok := call.Func.(*IdentExpr)
	if !ok {
		t.Fatalf("expected IdentExpr, got %T", call.Func)
	}
	if ident.Name != "print" {
		t.Errorf("expected 'print', got %q", ident.Name)
	}
	if len(call.Args) != 1 {
		t.Fatalf("expected 1 arg, got %d", len(call.Args))
	}
	intLit, ok := call.Args[0].(*IntLit)
	if !ok {
		t.Fatalf("expected IntLit, got %T", call.Args[0])
	}
	if intLit.Value != 42 {
		t.Errorf("expected 42, got %d", intLit.Value)
	}
}

func TestParseClassWithMethods(t *testing.T) {
	src := "class Point:\n    def __init__(self, x: int):\n        self.x = x\n    def get(self) -> int:\n        return self.x\n"
	prog := parseSource(t, src)
	if len(prog.Stmts) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(prog.Stmts))
	}
	cd, ok := prog.Stmts[0].(*ClassDef)
	if !ok {
		t.Fatalf("expected ClassDef, got %T", prog.Stmts[0])
	}
	if cd.Name != "Point" {
		t.Errorf("expected class name Point, got %q", cd.Name)
	}
	if cd.Base != "" {
		t.Errorf("expected no base, got %q", cd.Base)
	}
	if len(cd.Methods) != 2 {
		t.Fatalf("expected 2 methods, got %d", len(cd.Methods))
	}
	if cd.Methods[0].Name != "__init__" {
		t.Errorf("expected first method __init__, got %q", cd.Methods[0].Name)
	}
	if cd.Methods[0].Params[0].Name != "self" {
		t.Errorf("expected first param 'self', got %q", cd.Methods[0].Params[0].Name)
	}
	if cd.Methods[0].Params[0].TypeAnn != nil {
		t.Errorf("self should not have a type annotation")
	}
}

func TestParseClassWithBase(t *testing.T) {
	src := "class Dog(Animal):\n    def bark(self) -> None:\n        return\n"
	prog := parseSource(t, src)
	cd := prog.Stmts[0].(*ClassDef)
	if cd.Base != "Animal" {
		t.Errorf("expected base Animal, got %q", cd.Base)
	}
}

func TestParseSuperCall(t *testing.T) {
	src := "class Dog(Animal):\n    def __init__(self):\n        super().__init__()\n"
	prog := parseSource(t, src)
	cd := prog.Stmts[0].(*ClassDef)
	body := cd.Methods[0].Body
	if len(body) != 1 {
		t.Fatalf("expected 1 body statement, got %d", len(body))
	}
	call, ok := body[0].(*ExprStmt).Expr.(*CallExpr)
	if !ok {
		t.Fatalf("expected CallExpr, got %T", body[0].(*ExprStmt).Expr)
	}
	attr, ok := call.Func.(*AttrExpr)
	if !ok {
		t.Fatalf("expected AttrExpr for super().__init__, got %T", call.Func)
	}
	if _, ok := attr.Object.(*SuperExpr); !ok {
		t.Fatalf("expected SuperExpr as callee object, got %T", attr.Object)
	}
}
