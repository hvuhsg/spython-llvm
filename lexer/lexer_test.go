package lexer

import "testing"

func TestNewEmpty(t *testing.T) {
	l := New("")
	tokens, err := l.Tokens()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Type != TOKEN_EOF {
		t.Fatalf("expected single EOF token, got %v", tokens)
	}
}

func TestPrintInt(t *testing.T) {
	l := New("print(42)\n")
	tokens, err := l.Tokens()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []TokenType{TOKEN_IDENT, TOKEN_LPAREN, TOKEN_INT, TOKEN_RPAREN, TOKEN_NEWLINE, TOKEN_EOF}
	if len(tokens) != len(expected) {
		t.Fatalf("expected %d tokens, got %d: %v", len(expected), len(tokens), tokens)
	}
	for i, tok := range tokens {
		if tok.Type != expected[i] {
			t.Errorf("token %d: expected %s, got %s", i, expected[i], tok.Type)
		}
	}
	if tokens[0].Literal != "print" {
		t.Errorf("expected literal 'print', got %q", tokens[0].Literal)
	}
	if tokens[2].Literal != "42" {
		t.Errorf("expected literal '42', got %q", tokens[2].Literal)
	}
}

func TestArithmeticTokens(t *testing.T) {
	l := New("1 + 2 * 3\n")
	tokens, err := l.Tokens()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []TokenType{TOKEN_INT, TOKEN_PLUS, TOKEN_INT, TOKEN_STAR, TOKEN_INT, TOKEN_NEWLINE, TOKEN_EOF}
	if len(tokens) != len(expected) {
		t.Fatalf("expected %d tokens, got %d", len(expected), len(tokens))
	}
	for i, tok := range tokens {
		if tok.Type != expected[i] {
			t.Errorf("token %d: expected %s, got %s", i, expected[i], tok.Type)
		}
	}
}
