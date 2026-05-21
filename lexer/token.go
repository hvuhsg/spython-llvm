package lexer

type TokenType int

const (
	// Special
	TOKEN_EOF TokenType = iota
	TOKEN_NEWLINE
	TOKEN_INDENT
	TOKEN_DEDENT

	// Literals
	TOKEN_INT
	TOKEN_FLOAT
	TOKEN_STRING
	TOKEN_BYTES
	TOKEN_IDENT

	// Keywords
	TOKEN_IF
	TOKEN_ELIF
	TOKEN_ELSE
	TOKEN_WHILE
	TOKEN_FOR
	TOKEN_IN
	TOKEN_DEF
	TOKEN_LAMBDA
	TOKEN_RETURN
	TOKEN_AND
	TOKEN_OR
	TOKEN_NOT
	TOKEN_TRUE
	TOKEN_FALSE
	TOKEN_NONE
	TOKEN_RANGE
	TOKEN_BREAK
	TOKEN_CONTINUE
	TOKEN_IMPORT
	TOKEN_FROM
	TOKEN_AS
	TOKEN_CLASS
	TOKEN_TRY
	TOKEN_EXCEPT
	TOKEN_FINALLY
	TOKEN_RAISE
	TOKEN_YIELD

	// Operators
	TOKEN_PLUS     // +
	TOKEN_MINUS    // -
	TOKEN_STAR     // *
	TOKEN_SLASH    // /
	TOKEN_DSLASH   // //
	TOKEN_PERCENT  // %
	TOKEN_DSTAR    // **
	TOKEN_AMP      // &
	TOKEN_PIPE     // |
	TOKEN_CARET    // ^
	TOKEN_TILDE    // ~
	TOKEN_LSHIFT   // <<
	TOKEN_RSHIFT   // >>
	TOKEN_EQ       // ==
	TOKEN_NEQ      // !=
	TOKEN_LT       // <
	TOKEN_GT       // >
	TOKEN_LTE      // <=
	TOKEN_GTE      // >=
	TOKEN_ASSIGN   // =
	TOKEN_PLUSEQ    // +=
	TOKEN_MINUSEQ   // -=
	TOKEN_STAREQ    // *=
	TOKEN_SLASHEQ   // /=
	TOKEN_DSLASHEQ  // //=
	TOKEN_PERCENTEQ // %=
	TOKEN_DSTAREQ   // **=
	TOKEN_AMPEQ     // &=
	TOKEN_PIPEEQ    // |=
	TOKEN_CARETEQ   // ^=
	TOKEN_LSHIFTEQ  // <<=
	TOKEN_RSHIFTEQ  // >>=

	// Delimiters
	TOKEN_LPAREN // (
	TOKEN_RPAREN // )
	TOKEN_LBRACK // [
	TOKEN_RBRACK // ]
	TOKEN_LBRACE // {
	TOKEN_RBRACE // }
	TOKEN_COLON    // :
	TOKEN_COMMA    // ,
	TOKEN_DOT      // .
	TOKEN_ARROW    // ->
	TOKEN_AT       // @
	TOKEN_ELLIPSIS // ...
)

var tokenNames = map[TokenType]string{
	TOKEN_EOF:      "EOF",
	TOKEN_NEWLINE:  "NEWLINE",
	TOKEN_INDENT:   "INDENT",
	TOKEN_DEDENT:   "DEDENT",
	TOKEN_INT:      "INT",
	TOKEN_FLOAT:    "FLOAT",
	TOKEN_STRING:   "STRING",
	TOKEN_BYTES:    "BYTES",
	TOKEN_IDENT:    "IDENT",
	TOKEN_IF:       "if",
	TOKEN_ELIF:     "elif",
	TOKEN_ELSE:     "else",
	TOKEN_WHILE:    "while",
	TOKEN_FOR:      "for",
	TOKEN_IN:       "in",
	TOKEN_DEF:      "def",
	TOKEN_LAMBDA:   "lambda",
	TOKEN_RETURN:   "return",
	TOKEN_AND:      "and",
	TOKEN_OR:       "or",
	TOKEN_NOT:      "not",
	TOKEN_TRUE:     "True",
	TOKEN_FALSE:    "False",
	TOKEN_NONE:     "None",
	TOKEN_RANGE:    "range",
	TOKEN_BREAK:    "break",
	TOKEN_CONTINUE: "continue",
	TOKEN_IMPORT:   "import",
	TOKEN_FROM:     "from",
	TOKEN_AS:       "as",
	TOKEN_CLASS:    "class",
	TOKEN_TRY:      "try",
	TOKEN_EXCEPT:   "except",
	TOKEN_FINALLY:  "finally",
	TOKEN_RAISE:    "raise",
	TOKEN_YIELD:    "yield",
	TOKEN_PLUS:     "+",
	TOKEN_MINUS:    "-",
	TOKEN_STAR:     "*",
	TOKEN_SLASH:    "/",
	TOKEN_DSLASH:   "//",
	TOKEN_PERCENT:  "%",
	TOKEN_DSTAR:    "**",
	TOKEN_AMP:      "&",
	TOKEN_PIPE:     "|",
	TOKEN_CARET:    "^",
	TOKEN_TILDE:    "~",
	TOKEN_LSHIFT:   "<<",
	TOKEN_RSHIFT:   ">>",
	TOKEN_EQ:       "==",
	TOKEN_NEQ:      "!=",
	TOKEN_LT:       "<",
	TOKEN_GT:       ">",
	TOKEN_LTE:      "<=",
	TOKEN_GTE:      ">=",
	TOKEN_ASSIGN:   "=",
	TOKEN_PLUSEQ:    "+=",
	TOKEN_MINUSEQ:   "-=",
	TOKEN_STAREQ:    "*=",
	TOKEN_SLASHEQ:   "/=",
	TOKEN_DSLASHEQ:  "//=",
	TOKEN_PERCENTEQ: "%=",
	TOKEN_DSTAREQ:   "**=",
	TOKEN_AMPEQ:     "&=",
	TOKEN_PIPEEQ:    "|=",
	TOKEN_CARETEQ:   "^=",
	TOKEN_LSHIFTEQ:  "<<=",
	TOKEN_RSHIFTEQ:  ">>=",
	TOKEN_LPAREN:   "(",
	TOKEN_RPAREN:   ")",
	TOKEN_LBRACK:   "[",
	TOKEN_RBRACK:   "]",
	TOKEN_LBRACE:   "{",
	TOKEN_RBRACE:   "}",
	TOKEN_COLON:    ":",
	TOKEN_COMMA:    ",",
	TOKEN_DOT:      ".",
	TOKEN_ARROW:    "->",
	TOKEN_AT:       "@",
	TOKEN_ELLIPSIS: "...",
}

func (t TokenType) String() string {
	if name, ok := tokenNames[t]; ok {
		return name
	}
	return "UNKNOWN"
}

var keywords = map[string]TokenType{
	"if":       TOKEN_IF,
	"elif":     TOKEN_ELIF,
	"else":     TOKEN_ELSE,
	"while":    TOKEN_WHILE,
	"for":      TOKEN_FOR,
	"in":       TOKEN_IN,
	"def":      TOKEN_DEF,
	"lambda":   TOKEN_LAMBDA,
	"return":   TOKEN_RETURN,
	"and":      TOKEN_AND,
	"or":       TOKEN_OR,
	"not":      TOKEN_NOT,
	"True":     TOKEN_TRUE,
	"False":    TOKEN_FALSE,
	"None":     TOKEN_NONE,
	"range":    TOKEN_RANGE,
	"break":    TOKEN_BREAK,
	"continue": TOKEN_CONTINUE,
	"import":   TOKEN_IMPORT,
	"from":     TOKEN_FROM,
	"as":       TOKEN_AS,
	"class":    TOKEN_CLASS,
	"try":      TOKEN_TRY,
	"except":   TOKEN_EXCEPT,
	"finally":  TOKEN_FINALLY,
	"raise":    TOKEN_RAISE,
	"yield":    TOKEN_YIELD,
}

func LookupKeyword(ident string) TokenType {
	if tok, ok := keywords[ident]; ok {
		return tok
	}
	return TOKEN_IDENT
}

type Token struct {
	Type    TokenType
	Literal string
	Line    int
	Col     int
}
