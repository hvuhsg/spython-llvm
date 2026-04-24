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
