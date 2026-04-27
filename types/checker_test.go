package types

import (
	"strings"
	"testing"

	"github.com/yehoyadashtinmetz/spython/lexer"
	"github.com/yehoyadashtinmetz/spython/parser"
)

func TestNewChecker(t *testing.T) {
	c := NewChecker()
	err := c.Check(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func checkSource(t *testing.T, source string) error {
	t.Helper()
	l := lexer.New(source)
	tokens, err := l.Tokens()
	if err != nil {
		t.Fatalf("lexer error: %v", err)
	}
	p := parser.New(tokens)
	prog, err := p.Parse()
	if err != nil {
		t.Fatalf("parser error: %v", err)
	}
	c := NewChecker()
	return c.Check(prog)
}

func TestInferType_FromLiteral(t *testing.T) {
	if err := checkSource(t, "x = 42\ny = x + 1\n"); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
}

func TestInferType_RequiresAnnotationForNone(t *testing.T) {
	err := checkSource(t, "x = None\n")
	if err == nil {
		t.Fatalf("expected error inferring type from None, got none")
	}
	if !strings.Contains(err.Error(), "None") {
		t.Fatalf("expected error mentioning None, got: %v", err)
	}
}

func TestInferType_RequiresAnnotationForEmptyList(t *testing.T) {
	err := checkSource(t, "xs = []\n")
	if err == nil {
		t.Fatalf("expected error inferring type from empty list, got none")
	}
}

func TestInferType_RejectsReassignWithDifferentType(t *testing.T) {
	err := checkSource(t, "x = 1\nx = \"hi\"\n")
	if err == nil {
		t.Fatalf("expected type error on incompatible reassignment, got none")
	}
}

func TestVarArgs_TypeMismatch(t *testing.T) {
	err := checkSource(t, "def f(*xs: int) -> None: ...\nf(1, \"two\", 3)\n")
	if err == nil || !strings.Contains(err.Error(), "*args") {
		t.Fatalf("expected *args type error, got: %v", err)
	}
}

func TestKwargs_UnknownKeywordRejected(t *testing.T) {
	err := checkSource(t, "def f(a: int) -> None: ...\nf(a=1, b=2)\n")
	if err == nil || !strings.Contains(err.Error(), "unexpected keyword") {
		t.Fatalf("expected unexpected-keyword error, got: %v", err)
	}
}

func TestKwOnly_RequiresKeyword(t *testing.T) {
	err := checkSource(t, "def f(a: int, *xs: int, mode: str) -> None: ...\nf(1, 2, 3)\n")
	if err == nil || !strings.Contains(err.Error(), "missing argument for parameter mode") {
		t.Fatalf("expected missing kw-only argument error, got: %v", err)
	}
}

func TestStarUnpack_RequiresList(t *testing.T) {
	err := checkSource(t, "def f(*xs: int) -> None: ...\nx: int = 1\nf(*x)\n")
	if err == nil || !strings.Contains(err.Error(), "*unpack") {
		t.Fatalf("expected *unpack list-required error, got: %v", err)
	}
}

func TestDStarUnpack_RequiresStrKeys(t *testing.T) {
	err := checkSource(t, "def f(**kw: int) -> None: ...\nm: map[int, int] = {1: 2}\nf(**m)\n")
	if err == nil || !strings.Contains(err.Error(), "str keys") {
		t.Fatalf("expected str-keys error, got: %v", err)
	}
}

func TestVarArgsAndKwargs_Accepted(t *testing.T) {
	if err := checkSource(t, "def f(a: int, *xs: int, mode: str, **kw: int) -> None: ...\nf(1, 2, 3, mode=\"x\", k=9)\n"); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestDefaults_PositionalOmitted(t *testing.T) {
	src := "def f(a: int, b: int = 10) -> int:\n    return a + b\n" +
		"x: int = f(5)\n" +
		"y: int = f(5, 20)\n" +
		"z: int = f(5, b=99)\n"
	if err := checkSource(t, src); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestDefaults_KwOnlyOmitted(t *testing.T) {
	src := "def f(a: int, *xs: int, mode: str = \"fast\") -> None: ...\n" +
		"f(1, 2, 3)\n" +
		"f(1, mode=\"slow\")\n"
	if err := checkSource(t, src); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestDefaults_TypeMismatch(t *testing.T) {
	err := checkSource(t, "def f(x: int = \"hi\") -> None: ...\n")
	if err == nil || !strings.Contains(err.Error(), "default value") {
		t.Fatalf("expected default-type error, got: %v", err)
	}
}

func TestDefaults_NoneOnNonOptional(t *testing.T) {
	err := checkSource(t, "def f(x: int = None) -> None: ...\n")
	if err == nil || !strings.Contains(err.Error(), "default value") {
		t.Fatalf("expected None-not-assignable error, got: %v", err)
	}
}

func TestDefaults_UndefinedNameRejected(t *testing.T) {
	err := checkSource(t, "def f(a: int, b: int = a) -> int:\n    return b\n")
	if err == nil {
		t.Fatalf("expected error: default referencing parameter from same def must not resolve")
	}
}

func TestDefaults_RejectedOnExtern(t *testing.T) {
	err := checkSource(t, "@extern\ndef f(x: int = 1) -> int: ...\n")
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("expected @extern default rejection, got: %v", err)
	}
}

func TestDefaults_StillRequiresUnfilled(t *testing.T) {
	err := checkSource(t, "def f(a: int, b: int) -> int:\n    return a + b\n" +
		"x: int = f(1)\n")
	if err == nil || !strings.Contains(err.Error(), "missing argument") {
		t.Fatalf("expected missing-argument error for non-defaulted param, got: %v", err)
	}
}
