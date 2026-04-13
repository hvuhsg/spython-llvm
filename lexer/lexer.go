package lexer

import (
	"fmt"
	"unicode"
)

type Lexer struct {
	source  string
	tokens  []Token
	current int
	line    int
	col     int
	indents []int
	atLineStart bool
}

func New(source string) *Lexer {
	return &Lexer{
		source:      source,
		line:        1,
		col:         1,
		indents:     []int{0},
		atLineStart: true,
	}
}

func (l *Lexer) Tokens() ([]Token, error) {
	for !l.isAtEnd() {
		if l.atLineStart {
			l.handleIndentation()
			l.atLineStart = false
		}

		l.skipSpaces()
		if l.isAtEnd() {
			break
		}

		ch := l.peek()

		// Skip comments
		if ch == '#' {
			l.skipComment()
			continue
		}

		// Newline
		if ch == '\n' {
			l.addToken(TOKEN_NEWLINE, "\\n")
			l.advance()
			l.line++
			l.col = 1
			l.atLineStart = true
			continue
		}

		// String literals
		if ch == '"' || ch == '\'' {
			tok, err := l.readString(ch)
			if err != nil {
				return nil, err
			}
			l.tokens = append(l.tokens, tok)
			continue
		}

		// Numbers
		if unicode.IsDigit(rune(ch)) {
			l.readNumber()
			continue
		}

		// Identifiers and keywords
		if unicode.IsLetter(rune(ch)) || ch == '_' {
			l.readIdentifier()
			continue
		}

		// Operators and delimiters
		if err := l.readOperator(); err != nil {
			return nil, err
		}
	}

	// Emit remaining DEDENTs
	for len(l.indents) > 1 {
		l.addToken(TOKEN_DEDENT, "")
		l.indents = l.indents[:len(l.indents)-1]
	}

	l.addToken(TOKEN_EOF, "")
	return l.tokens, nil
}

func (l *Lexer) handleIndentation() {
	indent := 0
	for !l.isAtEnd() && l.peek() == ' ' {
		indent++
		l.advance()
	}
	// Tab handling
	for !l.isAtEnd() && l.peek() == '\t' {
		indent += 4
		l.advance()
	}

	// Skip blank lines and comment-only lines
	if l.isAtEnd() || l.peek() == '\n' || l.peek() == '#' {
		return
	}

	currentIndent := l.indents[len(l.indents)-1]

	if indent > currentIndent {
		l.indents = append(l.indents, indent)
		l.addToken(TOKEN_INDENT, "")
	} else if indent < currentIndent {
		for len(l.indents) > 1 && l.indents[len(l.indents)-1] > indent {
			l.indents = l.indents[:len(l.indents)-1]
			l.addToken(TOKEN_DEDENT, "")
		}
	}
}

func (l *Lexer) skipSpaces() {
	for !l.isAtEnd() && (l.peek() == ' ' || l.peek() == '\t' || l.peek() == '\r') {
		l.advance()
	}
}

func (l *Lexer) skipComment() {
	for !l.isAtEnd() && l.peek() != '\n' {
		l.advance()
	}
}

func (l *Lexer) readString(quote byte) (Token, error) {
	startLine := l.line
	startCol := l.col
	l.advance() // consume opening quote
	result := ""
	for !l.isAtEnd() && l.peek() != quote {
		if l.peek() == '\n' {
			return Token{}, fmt.Errorf("%d:%d: unterminated string", startLine, startCol)
		}
		if l.peek() == '\\' {
			l.advance()
			if l.isAtEnd() {
				return Token{}, fmt.Errorf("%d:%d: unterminated string escape", startLine, startCol)
			}
			switch l.peek() {
			case 'n':
				result += "\n"
			case 't':
				result += "\t"
			case '\\':
				result += "\\"
			case '\'':
				result += "'"
			case '"':
				result += "\""
			default:
				result += "\\" + string(l.peek())
			}
			l.advance()
		} else {
			result += string(l.peek())
			l.advance()
		}
	}
	if l.isAtEnd() {
		return Token{}, fmt.Errorf("%d:%d: unterminated string", startLine, startCol)
	}
	l.advance() // consume closing quote
	return Token{Type: TOKEN_STRING, Literal: result, Line: startLine, Col: startCol}, nil
}

func (l *Lexer) readNumber() {
	startCol := l.col
	num := ""
	isFloat := false
	for !l.isAtEnd() && (unicode.IsDigit(rune(l.peek())) || l.peek() == '.') {
		if l.peek() == '.' {
			if isFloat {
				break
			}
			isFloat = true
		}
		num += string(l.peek())
		l.advance()
	}
	if isFloat {
		l.tokens = append(l.tokens, Token{Type: TOKEN_FLOAT, Literal: num, Line: l.line, Col: startCol})
	} else {
		l.tokens = append(l.tokens, Token{Type: TOKEN_INT, Literal: num, Line: l.line, Col: startCol})
	}
}

func (l *Lexer) readIdentifier() {
	startCol := l.col
	ident := ""
	for !l.isAtEnd() && (unicode.IsLetter(rune(l.peek())) || unicode.IsDigit(rune(l.peek())) || l.peek() == '_') {
		ident += string(l.peek())
		l.advance()
	}
	tokType := LookupKeyword(ident)
	l.tokens = append(l.tokens, Token{Type: tokType, Literal: ident, Line: l.line, Col: startCol})
}

func (l *Lexer) readOperator() error {
	startCol := l.col
	ch := l.peek()

	remaining := len(l.source) - l.current

	// Three-character operators (must check before two-char)
	if remaining >= 3 {
		three := l.source[l.current : l.current+3]
		switch three {
		case "**=":
			l.advance(); l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_DSTAREQ, Literal: "**=", Line: l.line, Col: startCol})
			return nil
		case "//=":
			l.advance(); l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_DSLASHEQ, Literal: "//=", Line: l.line, Col: startCol})
			return nil
		case "<<=":
			l.advance(); l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_LSHIFTEQ, Literal: "<<=", Line: l.line, Col: startCol})
			return nil
		case ">>=":
			l.advance(); l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_RSHIFTEQ, Literal: ">>=", Line: l.line, Col: startCol})
			return nil
		}
	}

	// Two-character operators
	if remaining >= 2 {
		two := l.source[l.current : l.current+2]
		switch two {
		case "**":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_DSTAR, Literal: "**", Line: l.line, Col: startCol})
			return nil
		case "//":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_DSLASH, Literal: "//", Line: l.line, Col: startCol})
			return nil
		case "==":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_EQ, Literal: "==", Line: l.line, Col: startCol})
			return nil
		case "!=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_NEQ, Literal: "!=", Line: l.line, Col: startCol})
			return nil
		case "<=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_LTE, Literal: "<=", Line: l.line, Col: startCol})
			return nil
		case ">=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_GTE, Literal: ">=", Line: l.line, Col: startCol})
			return nil
		case "+=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_PLUSEQ, Literal: "+=", Line: l.line, Col: startCol})
			return nil
		case "-=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_MINUSEQ, Literal: "-=", Line: l.line, Col: startCol})
			return nil
		case "*=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_STAREQ, Literal: "*=", Line: l.line, Col: startCol})
			return nil
		case "/=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_SLASHEQ, Literal: "/=", Line: l.line, Col: startCol})
			return nil
		case "%=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_PERCENTEQ, Literal: "%=", Line: l.line, Col: startCol})
			return nil
		case "&=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_AMPEQ, Literal: "&=", Line: l.line, Col: startCol})
			return nil
		case "|=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_PIPEEQ, Literal: "|=", Line: l.line, Col: startCol})
			return nil
		case "^=":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_CARETEQ, Literal: "^=", Line: l.line, Col: startCol})
			return nil
		case "<<":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_LSHIFT, Literal: "<<", Line: l.line, Col: startCol})
			return nil
		case ">>":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_RSHIFT, Literal: ">>", Line: l.line, Col: startCol})
			return nil
		case "->":
			l.advance(); l.advance()
			l.tokens = append(l.tokens, Token{Type: TOKEN_ARROW, Literal: "->", Line: l.line, Col: startCol})
			return nil
		}
	}

	// Single-character operators
	l.advance()
	switch ch {
	case '+':
		l.addTokenAt(TOKEN_PLUS, "+", startCol)
	case '-':
		l.addTokenAt(TOKEN_MINUS, "-", startCol)
	case '*':
		l.addTokenAt(TOKEN_STAR, "*", startCol)
	case '/':
		l.addTokenAt(TOKEN_SLASH, "/", startCol)
	case '%':
		l.addTokenAt(TOKEN_PERCENT, "%", startCol)
	case '&':
		l.addTokenAt(TOKEN_AMP, "&", startCol)
	case '|':
		l.addTokenAt(TOKEN_PIPE, "|", startCol)
	case '^':
		l.addTokenAt(TOKEN_CARET, "^", startCol)
	case '~':
		l.addTokenAt(TOKEN_TILDE, "~", startCol)
	case '<':
		l.addTokenAt(TOKEN_LT, "<", startCol)
	case '>':
		l.addTokenAt(TOKEN_GT, ">", startCol)
	case '=':
		l.addTokenAt(TOKEN_ASSIGN, "=", startCol)
	case '(':
		l.addTokenAt(TOKEN_LPAREN, "(", startCol)
	case ')':
		l.addTokenAt(TOKEN_RPAREN, ")", startCol)
	case '[':
		l.addTokenAt(TOKEN_LBRACK, "[", startCol)
	case ']':
		l.addTokenAt(TOKEN_RBRACK, "]", startCol)
	case '{':
		l.addTokenAt(TOKEN_LBRACE, "{", startCol)
	case '}':
		l.addTokenAt(TOKEN_RBRACE, "}", startCol)
	case ':':
		l.addTokenAt(TOKEN_COLON, ":", startCol)
	case ',':
		l.addTokenAt(TOKEN_COMMA, ",", startCol)
	case '.':
		l.addTokenAt(TOKEN_DOT, ".", startCol)
	default:
		return fmt.Errorf("%d:%d: unexpected character: %c", l.line, startCol, ch)
	}
	return nil
}

func (l *Lexer) isAtEnd() bool {
	return l.current >= len(l.source)
}

func (l *Lexer) peek() byte {
	return l.source[l.current]
}

func (l *Lexer) advance() byte {
	ch := l.source[l.current]
	l.current++
	l.col++
	return ch
}

func (l *Lexer) addToken(t TokenType, literal string) {
	l.tokens = append(l.tokens, Token{Type: t, Literal: literal, Line: l.line, Col: l.col})
}

func (l *Lexer) addTokenAt(t TokenType, literal string, col int) {
	l.tokens = append(l.tokens, Token{Type: t, Literal: literal, Line: l.line, Col: col})
}
