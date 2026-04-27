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

func TestParseVarArgsAndKwargsDef(t *testing.T) {
	src := "def f(a: int, *xs: int, mode: str, **kw: int) -> None:\n    return\n"
	prog := parseSource(t, src)
	fd, ok := prog.Stmts[0].(*FuncDef)
	if !ok {
		t.Fatalf("expected FuncDef, got %T", prog.Stmts[0])
	}
	if len(fd.Params) != 4 {
		t.Fatalf("expected 4 params, got %d", len(fd.Params))
	}
	wantKinds := []ParamKind{ParamPositional, ParamVarArgs, ParamPositional, ParamKwargs}
	wantNames := []string{"a", "xs", "mode", "kw"}
	for i, p := range fd.Params {
		if p.Kind != wantKinds[i] {
			t.Fatalf("param %d: expected Kind %d, got %d", i, wantKinds[i], p.Kind)
		}
		if p.Name != wantNames[i] {
			t.Fatalf("param %d: expected name %q, got %q", i, wantNames[i], p.Name)
		}
	}
}

func TestParseCallWithKwargsAndUnpacks(t *testing.T) {
	src := "f(1, *xs, k=2, **m)\n"
	prog := parseSource(t, src)
	call, ok := prog.Stmts[0].(*ExprStmt).Expr.(*CallExpr)
	if !ok {
		t.Fatalf("expected CallExpr, got %T", prog.Stmts[0].(*ExprStmt).Expr)
	}
	if len(call.Args) != 2 {
		t.Fatalf("expected 2 positional args, got %d", len(call.Args))
	}
	if !call.IsArgStar(1) || call.IsArgStar(0) {
		t.Fatalf("expected only arg[1] to be *unpack, got argStar=%v", call.ArgStar)
	}
	if len(call.Kwargs) != 2 {
		t.Fatalf("expected 2 kwargs, got %d", len(call.Kwargs))
	}
	if call.Kwargs[0].Name != "k" || call.Kwargs[0].IsDStar {
		t.Fatalf("expected first kwarg k=, got %+v", call.Kwargs[0])
	}
	if !call.Kwargs[1].IsDStar {
		t.Fatalf("expected second kwarg to be **unpack, got %+v", call.Kwargs[1])
	}
}

func TestParseRejectsPositionalAfterKwarg(t *testing.T) {
	src := "f(a=1, 2)\n"
	l := lexer.New(src)
	tokens, err := l.Tokens()
	if err != nil {
		t.Fatal(err)
	}
	p := New(tokens)
	if _, err := p.Parse(); err == nil {
		t.Fatalf("expected parse error for positional after kwarg")
	}
}

func TestParseDefaultArguments(t *testing.T) {
	src := "def f(a: int, b: int = 10, c: str = \"hi\") -> None:\n    return\n"
	prog := parseSource(t, src)
	fd, ok := prog.Stmts[0].(*FuncDef)
	if !ok {
		t.Fatalf("expected FuncDef, got %T", prog.Stmts[0])
	}
	if len(fd.Params) != 3 {
		t.Fatalf("expected 3 params, got %d", len(fd.Params))
	}
	if fd.Params[0].Default != nil {
		t.Fatalf("expected param 0 (a) to have no default")
	}
	intLit, ok := fd.Params[1].Default.(*IntLit)
	if !ok || intLit.Value != 10 {
		t.Fatalf("expected param 1 default IntLit(10), got %+v", fd.Params[1].Default)
	}
	strLit, ok := fd.Params[2].Default.(*StrLit)
	if !ok || strLit.Value != "hi" {
		t.Fatalf("expected param 2 default StrLit(\"hi\"), got %+v", fd.Params[2].Default)
	}
}

func TestParseDefaultsWithVarArgsAndKwOnly(t *testing.T) {
	src := "def f(a: int = 1, *xs: int, mode: str = \"fast\", **kw: int) -> None:\n    return\n"
	prog := parseSource(t, src)
	fd, ok := prog.Stmts[0].(*FuncDef)
	if !ok {
		t.Fatalf("expected FuncDef, got %T", prog.Stmts[0])
	}
	if len(fd.Params) != 4 {
		t.Fatalf("expected 4 params, got %d", len(fd.Params))
	}
	if fd.Params[0].Default == nil {
		t.Fatalf("expected positional param 'a' to have a default")
	}
	if fd.Params[1].Default != nil {
		t.Fatalf("*args must not have a default")
	}
	if fd.Params[2].Default == nil {
		t.Fatalf("expected kw-only param 'mode' to have a default")
	}
	if fd.Params[3].Default != nil {
		t.Fatalf("**kwargs must not have a default")
	}
}

func TestParseRejectsRequiredAfterDefault(t *testing.T) {
	src := "def f(a: int = 1, b: int) -> None:\n    return\n"
	l := lexer.New(src)
	tokens, err := l.Tokens()
	if err != nil {
		t.Fatal(err)
	}
	p := New(tokens)
	if _, err := p.Parse(); err == nil {
		t.Fatalf("expected parse error for required positional after defaulted positional")
	}
}

func TestParseRejectsDefaultOnVarArgs(t *testing.T) {
	src := "def f(*xs: int = 1) -> None:\n    return\n"
	l := lexer.New(src)
	tokens, err := l.Tokens()
	if err != nil {
		t.Fatal(err)
	}
	p := New(tokens)
	if _, err := p.Parse(); err == nil {
		t.Fatalf("expected parse error for default on *args")
	}
}
