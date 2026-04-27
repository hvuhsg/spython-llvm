package parser

import (
	"fmt"
	"strconv"

	"github.com/yehoyadashtinmetz/spython/lexer"
)

type Parser struct {
	tokens  []lexer.Token
	current int
	file    string
}

func New(tokens []lexer.Token) *Parser {
	return &Parser{tokens: tokens}
}

func (p *Parser) SetFile(file string) {
	p.file = file
}

func (p *Parser) Parse() (*Program, error) {
	stmts := []Stmt{}
	for !p.isAtEnd() {
		p.skipNewlines()
		if p.isAtEnd() {
			break
		}
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		if stmt != nil {
			stmts = append(stmts, stmt)
		}
	}
	return &Program{Stmts: stmts}, nil
}

func (p *Parser) parseStatement() (Stmt, error) {
	tok := p.peek()

	switch tok.Type {
	case lexer.TOKEN_AT:
		return p.parseDecoratedFuncDef()
	case lexer.TOKEN_IF:
		return p.parseIfStmt()
	case lexer.TOKEN_WHILE:
		return p.parseWhileStmt()
	case lexer.TOKEN_FOR:
		return p.parseForStmt()
	case lexer.TOKEN_DEF:
		return p.parseFuncDef()
	case lexer.TOKEN_RETURN:
		return p.parseReturnStmt()
	case lexer.TOKEN_BREAK:
		p.advance()
		p.consumeNewline()
		return &BreakStmt{Pos: p.makePos(tok)}, nil
	case lexer.TOKEN_CONTINUE:
		p.advance()
		p.consumeNewline()
		return &ContinueStmt{Pos: p.makePos(tok)}, nil
	case lexer.TOKEN_IMPORT:
		return p.parseImportStmt()
	case lexer.TOKEN_FROM:
		return p.parseFromImportStmt()
	case lexer.TOKEN_CLASS:
		return p.parseClassDef()
	case lexer.TOKEN_TRY:
		return p.parseTryStmt()
	case lexer.TOKEN_RAISE:
		return p.parseRaiseStmt()
	}

	// Could be assignment (x: type = expr) or expression statement
	// Look ahead to determine
	if tok.Type == lexer.TOKEN_IDENT {
		// Check for assignment: IDENT COLON type ASSIGN expr
		// or augmented assignment: IDENT += expr
		if p.peekN(1).Type == lexer.TOKEN_COLON {
			return p.parseAssignStmt()
		}
		if isAugAssign(p.peekN(1).Type) {
			return p.parseAugAssignStmt()
		}
		// Multi-assign: IDENT COMMA IDENT [...COMMA IDENT] ASSIGN expr
		if p.peekN(1).Type == lexer.TOKEN_COMMA && p.peekN(2).Type == lexer.TOKEN_IDENT {
			return p.parseMultiAssignStmt()
		}
	}

	// Expression statement (may also be index assignment)
	expr, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}

	// Check for index assignment: expr[idx] = value
	if idx, ok := expr.(*IndexExpr); ok && p.peek().Type == lexer.TOKEN_ASSIGN {
		p.advance() // consume =
		value, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		p.consumeNewline()
		return &IndexAssignStmt{
			Pos:    idx.Pos,
			Object: idx.Object,
			Index:  idx.Index,
			Value:  value,
		}, nil
	}

	// Check for attribute assignment: obj.attr = value or obj.attr: T = value
	if attr, ok := expr.(*AttrExpr); ok {
		if p.peek().Type == lexer.TOKEN_ASSIGN {
			p.advance() // consume =
			value, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			p.consumeNewline()
			return &AttrAssignStmt{
				Pos:    attr.Pos,
				Object: attr.Object,
				Attr:   attr.Attr,
				Value:  value,
			}, nil
		}
		if p.peek().Type == lexer.TOKEN_COLON {
			p.advance() // consume :
			typeAnn, err := p.parseTypeAnnotation()
			if err != nil {
				return nil, err
			}
			if err := p.expect(lexer.TOKEN_ASSIGN); err != nil {
				return nil, err
			}
			value, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			p.consumeNewline()
			return &AttrAssignStmt{
				Pos:     attr.Pos,
				Object:  attr.Object,
				Attr:    attr.Attr,
				TypeAnn: typeAnn,
				Value:   value,
			}, nil
		}
	}

	// Check for reassignment: ident = expr (without type annotation)
	if ident, ok := expr.(*IdentExpr); ok && p.peek().Type == lexer.TOKEN_ASSIGN {
		p.advance() // consume =
		value, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		p.consumeNewline()
		return &AssignStmt{
			Pos:   ident.Pos,
			Name:  ident.Name,
			Value: value,
		}, nil
	}

	p.consumeNewline()
	return &ExprStmt{Pos: expr.GetPos(), Expr: expr}, nil
}

func isAugAssign(t lexer.TokenType) bool {
	switch t {
	case lexer.TOKEN_PLUSEQ, lexer.TOKEN_MINUSEQ, lexer.TOKEN_STAREQ, lexer.TOKEN_SLASHEQ,
		lexer.TOKEN_DSLASHEQ, lexer.TOKEN_PERCENTEQ, lexer.TOKEN_DSTAREQ,
		lexer.TOKEN_AMPEQ, lexer.TOKEN_PIPEEQ, lexer.TOKEN_CARETEQ,
		lexer.TOKEN_LSHIFTEQ, lexer.TOKEN_RSHIFTEQ:
		return true
	}
	return false
}

// parseMultiAssignStmt handles `a, b[, c] = expr` where each LHS is a bare
// identifier. Requires at least two names; single-name assignments go through
// parseAssignStmt (with type annotation) or the IdentExpr path in
// parseStatement (reassignment).
func (p *Parser) parseMultiAssignStmt() (Stmt, error) {
	firstTok := p.advance() // first IDENT
	names := []string{firstTok.Literal}
	for p.peek().Type == lexer.TOKEN_COMMA {
		p.advance() // consume comma
		nameTok := p.peek()
		if nameTok.Type != lexer.TOKEN_IDENT {
			return nil, fmt.Errorf("%d:%d: expected identifier in multi-assign, got %s",
				nameTok.Line, nameTok.Col, nameTok.Type)
		}
		p.advance()
		names = append(names, nameTok.Literal)
	}
	if err := p.expect(lexer.TOKEN_ASSIGN); err != nil {
		return nil, err
	}
	value, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	p.consumeNewline()
	return &MultiAssignStmt{
		Pos:   p.makePos(firstTok),
		Names: names,
		Value: value,
	}, nil
}

func (p *Parser) parseAssignStmt() (Stmt, error) {
	nameTok := p.advance() // IDENT
	p.advance()            // COLON

	typeAnn, err := p.parseTypeAnnotation()
	if err != nil {
		return nil, err
	}

	if err := p.expect(lexer.TOKEN_ASSIGN); err != nil {
		return nil, err
	}

	value, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}

	p.consumeNewline()
	return &AssignStmt{
		Pos:     p.makePos(nameTok),
		Name:    nameTok.Literal,
		TypeAnn: typeAnn,
		Value:   value,
	}, nil
}

func (p *Parser) parseAugAssignStmt() (Stmt, error) {
	nameTok := p.advance() // IDENT
	opTok := p.advance()   // +=, -=, etc.

	var op string
	switch opTok.Type {
	case lexer.TOKEN_PLUSEQ:
		op = "+"
	case lexer.TOKEN_MINUSEQ:
		op = "-"
	case lexer.TOKEN_STAREQ:
		op = "*"
	case lexer.TOKEN_SLASHEQ:
		op = "/"
	case lexer.TOKEN_DSLASHEQ:
		op = "//"
	case lexer.TOKEN_PERCENTEQ:
		op = "%"
	case lexer.TOKEN_DSTAREQ:
		op = "**"
	case lexer.TOKEN_AMPEQ:
		op = "&"
	case lexer.TOKEN_PIPEEQ:
		op = "|"
	case lexer.TOKEN_CARETEQ:
		op = "^"
	case lexer.TOKEN_LSHIFTEQ:
		op = "<<"
	case lexer.TOKEN_RSHIFTEQ:
		op = ">>"
	}

	value, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}

	p.consumeNewline()
	return &AugAssignStmt{
		Pos:   p.makePos(nameTok),
		Name:  nameTok.Literal,
		Op:    op,
		Value: value,
	}, nil
}

func (p *Parser) parseTypeAnnotation() (*TypeAnnotation, error) {
	nameTok := p.peek()
	if nameTok.Type != lexer.TOKEN_IDENT && nameTok.Type != lexer.TOKEN_NONE {
		return nil, fmt.Errorf("%d:%d: expected type name, got %s", nameTok.Line, nameTok.Col, nameTok.Type)
	}
	p.advance()

	ann := &TypeAnnotation{
		Pos:  p.makePos(nameTok),
		Name: nameTok.Literal,
	}

	// Check for generic params: list[int], map[str, int]
	if p.peek().Type == lexer.TOKEN_LBRACK {
		p.advance() // consume [
		for {
			param, err := p.parseTypeAnnotation()
			if err != nil {
				return nil, err
			}
			ann.Params = append(ann.Params, param)
			if p.peek().Type != lexer.TOKEN_COMMA {
				break
			}
			p.advance()
			if p.peek().Type == lexer.TOKEN_RBRACK {
				break // trailing comma
			}
		}
		if err := p.expect(lexer.TOKEN_RBRACK); err != nil {
			return nil, err
		}
	}

	return ann, nil
}

// Precedence climbing expression parser
func (p *Parser) parseExpr(minPrec int) (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}

	for {
		tok := p.peek()
		prec, ok := binaryPrec(tok.Type)
		if !ok || prec < minPrec {
			break
		}

		op := tok.Literal
		p.advance()

		// Right-associative for **
		nextPrec := prec + 1
		if tok.Type == lexer.TOKEN_DSTAR {
			nextPrec = prec
		}

		right, err := p.parseExpr(nextPrec)
		if err != nil {
			return nil, err
		}

		left = &BinaryExpr{
			Pos:   left.GetPos(),
			Left:  left,
			Op:    op,
			Right: right,
		}
	}

	return left, nil
}

func binaryPrec(t lexer.TokenType) (int, bool) {
	// Python precedence (low to high):
	// or(1) < and(2) < [not is unary] < comparisons(3) < |(4) < ^(5) < &(6) < shifts(7) < +-(8) < */%//(9) < **(10)
	switch t {
	case lexer.TOKEN_OR:
		return 1, true
	case lexer.TOKEN_AND:
		return 2, true
	case lexer.TOKEN_EQ, lexer.TOKEN_NEQ, lexer.TOKEN_LT, lexer.TOKEN_GT, lexer.TOKEN_LTE, lexer.TOKEN_GTE:
		return 3, true
	case lexer.TOKEN_PIPE:
		return 4, true
	case lexer.TOKEN_CARET:
		return 5, true
	case lexer.TOKEN_AMP:
		return 6, true
	case lexer.TOKEN_LSHIFT, lexer.TOKEN_RSHIFT:
		return 7, true
	case lexer.TOKEN_PLUS, lexer.TOKEN_MINUS:
		return 8, true
	case lexer.TOKEN_STAR, lexer.TOKEN_SLASH, lexer.TOKEN_DSLASH, lexer.TOKEN_PERCENT:
		return 9, true
	case lexer.TOKEN_DSTAR:
		return 10, true
	}
	return 0, false
}

func (p *Parser) parseUnary() (Expr, error) {
	tok := p.peek()
	// 'not' has low precedence (below comparisons), handled in parseExpr via prefix check
	if tok.Type == lexer.TOKEN_NOT {
		p.advance()
		// 'not' has precedence above 'and'(2) but below comparisons(3)
		operand, err := p.parseExpr(3)
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{
			Pos:     p.makePos(tok),
			Op:      tok.Literal,
			Operand: operand,
		}, nil
	}
	if tok.Type == lexer.TOKEN_MINUS || tok.Type == lexer.TOKEN_TILDE {
		p.advance()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{
			Pos:     p.makePos(tok),
			Op:      tok.Literal,
			Operand: operand,
		}, nil
	}
	return p.parsePostfix()
}

func (p *Parser) parsePostfix() (Expr, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	for {
		tok := p.peek()
		switch tok.Type {
		case lexer.TOKEN_LPAREN:
			// Function call
			p.advance()
			args := []Expr{}
			if p.peek().Type != lexer.TOKEN_RPAREN {
				for {
					arg, err := p.parseExpr(0)
					if err != nil {
						return nil, err
					}
					args = append(args, arg)
					if p.peek().Type != lexer.TOKEN_COMMA {
						break
					}
					p.advance()
					if p.peek().Type == lexer.TOKEN_RPAREN {
						break // trailing comma
					}
				}
			}
			if err := p.expect(lexer.TOKEN_RPAREN); err != nil {
				return nil, err
			}
			// Recognize `super()` (zero-arg) as the SuperExpr sentinel so it
			// can appear as the receiver of `.method(...)` in a class body.
			if ident, ok := expr.(*IdentExpr); ok && ident.Name == "super" && len(args) == 0 {
				expr = &SuperExpr{Pos: ident.Pos}
				break
			}
			expr = &CallExpr{
				Pos:  expr.GetPos(),
				Func: expr,
				Args: args,
			}
		case lexer.TOKEN_LBRACK:
			// Index
			p.advance()
			index, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			if err := p.expect(lexer.TOKEN_RBRACK); err != nil {
				return nil, err
			}
			expr = &IndexExpr{
				Pos:    expr.GetPos(),
				Object: expr,
				Index:  index,
			}
		case lexer.TOKEN_DOT:
			// Attribute access
			p.advance()
			attrTok := p.peek()
			if attrTok.Type != lexer.TOKEN_IDENT {
				return nil, fmt.Errorf("%d:%d: expected attribute name, got %s", attrTok.Line, attrTok.Col, attrTok.Type)
			}
			p.advance()
			expr = &AttrExpr{
				Pos:    expr.GetPos(),
				Object: expr,
				Attr:   attrTok.Literal,
			}
		default:
			return expr, nil
		}
	}
}

func (p *Parser) parsePrimary() (Expr, error) {
	tok := p.peek()

	switch tok.Type {
	case lexer.TOKEN_INT:
		p.advance()
		val, err := strconv.ParseInt(tok.Literal, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%d:%d: invalid integer: %s", tok.Line, tok.Col, tok.Literal)
		}
		return &IntLit{Pos: p.makePos(tok), Value: val}, nil

	case lexer.TOKEN_FLOAT:
		p.advance()
		val, err := strconv.ParseFloat(tok.Literal, 64)
		if err != nil {
			return nil, fmt.Errorf("%d:%d: invalid float: %s", tok.Line, tok.Col, tok.Literal)
		}
		return &FloatLit{Pos: p.makePos(tok), Value: val}, nil

	case lexer.TOKEN_STRING:
		p.advance()
		return &StrLit{Pos: p.makePos(tok), Value: tok.Literal}, nil

	case lexer.TOKEN_BYTES:
		p.advance()
		return &BytesLit{Pos: p.makePos(tok), Value: tok.Literal}, nil

	case lexer.TOKEN_TRUE:
		p.advance()
		return &BoolLit{Pos: p.makePos(tok), Value: true}, nil

	case lexer.TOKEN_FALSE:
		p.advance()
		return &BoolLit{Pos: p.makePos(tok), Value: false}, nil

	case lexer.TOKEN_NONE:
		p.advance()
		return &NoneLit{Pos: p.makePos(tok)}, nil

	case lexer.TOKEN_IDENT, lexer.TOKEN_RANGE:
		p.advance()
		return &IdentExpr{Pos: p.makePos(tok), Name: tok.Literal}, nil

	case lexer.TOKEN_LPAREN:
		lparenTok := p.advance()
		// Empty tuple: ()
		if p.peek().Type == lexer.TOKEN_RPAREN {
			p.advance()
			return &TupleLit{Pos: p.makePos(lparenTok), Elements: nil}, nil
		}
		first, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		// Not a comma -> parenthesized expression (grouping).
		if p.peek().Type != lexer.TOKEN_COMMA {
			if err := p.expect(lexer.TOKEN_RPAREN); err != nil {
				return nil, err
			}
			return first, nil
		}
		// Tuple: (x, ...) or (x,)
		elements := []Expr{first}
		for p.peek().Type == lexer.TOKEN_COMMA {
			p.advance() // consume comma
			if p.peek().Type == lexer.TOKEN_RPAREN {
				break // trailing comma (also terminates 1-tuple form `(x,)`)
			}
			next, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			elements = append(elements, next)
		}
		if err := p.expect(lexer.TOKEN_RPAREN); err != nil {
			return nil, err
		}
		return &TupleLit{Pos: p.makePos(lparenTok), Elements: elements}, nil

	case lexer.TOKEN_LBRACK:
		return p.parseListLit()

	case lexer.TOKEN_LBRACE:
		return p.parseMapLit()

	default:
		return nil, fmt.Errorf("%d:%d: unexpected token: %s (%q)", tok.Line, tok.Col, tok.Type, tok.Literal)
	}
}

func (p *Parser) parseListLit() (Expr, error) {
	tok := p.advance() // consume [
	elements := []Expr{}
	if p.peek().Type != lexer.TOKEN_RBRACK {
		for {
			elem, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			elements = append(elements, elem)
			if p.peek().Type != lexer.TOKEN_COMMA {
				break
			}
			p.advance()
			if p.peek().Type == lexer.TOKEN_RBRACK {
				break // trailing comma
			}
		}
	}
	if err := p.expect(lexer.TOKEN_RBRACK); err != nil {
		return nil, err
	}
	return &ListLit{Pos: p.makePos(tok), Elements: elements}, nil
}

func (p *Parser) parseMapLit() (Expr, error) {
	tok := p.advance() // consume {
	keys := []Expr{}
	values := []Expr{}
	if p.peek().Type != lexer.TOKEN_RBRACE {
		for {
			key, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			if err := p.expect(lexer.TOKEN_COLON); err != nil {
				return nil, err
			}
			val, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			keys = append(keys, key)
			values = append(values, val)
			if p.peek().Type != lexer.TOKEN_COMMA {
				break
			}
			p.advance()
			if p.peek().Type == lexer.TOKEN_RBRACE {
				break // trailing comma
			}
		}
	}
	if err := p.expect(lexer.TOKEN_RBRACE); err != nil {
		return nil, err
	}
	return &MapLit{Pos: p.makePos(tok), Keys: keys, Values: values}, nil
}

func (p *Parser) parseIfStmt() (Stmt, error) {
	tok := p.advance() // consume 'if'
	cond, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expect(lexer.TOKEN_COLON); err != nil {
		return nil, err
	}
	p.consumeNewline()

	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	var elifs []ElifClause
	for p.peek().Type == lexer.TOKEN_ELIF {
		elifTok := p.advance()
		elifCond, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if err := p.expect(lexer.TOKEN_COLON); err != nil {
			return nil, err
		}
		p.consumeNewline()
		elifBody, err := p.parseBlock()
		if err != nil {
			return nil, err
		}
		elifs = append(elifs, ElifClause{
			Pos:       p.makePos(elifTok),
			Condition: elifCond,
			Body:      elifBody,
		})
	}

	var elseBody []Stmt
	if p.peek().Type == lexer.TOKEN_ELSE {
		p.advance()
		if err := p.expect(lexer.TOKEN_COLON); err != nil {
			return nil, err
		}
		p.consumeNewline()
		elseBody, err = p.parseBlock()
		if err != nil {
			return nil, err
		}
	}

	return &IfStmt{
		Pos:       p.makePos(tok),
		Condition: cond,
		Body:      body,
		Elifs:     elifs,
		ElseBody:  elseBody,
	}, nil
}

func (p *Parser) parseWhileStmt() (Stmt, error) {
	tok := p.advance() // consume 'while'
	cond, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expect(lexer.TOKEN_COLON); err != nil {
		return nil, err
	}
	p.consumeNewline()

	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	return &WhileStmt{
		Pos:       p.makePos(tok),
		Condition: cond,
		Body:      body,
	}, nil
}

func (p *Parser) parseForStmt() (Stmt, error) {
	tok := p.advance() // consume 'for'

	varTok := p.peek()
	if varTok.Type != lexer.TOKEN_IDENT {
		return nil, fmt.Errorf("%d:%d: expected identifier, got %s", varTok.Line, varTok.Col, varTok.Type)
	}
	p.advance()

	if err := p.expect(lexer.TOKEN_IN); err != nil {
		return nil, err
	}

	iter, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}

	if err := p.expect(lexer.TOKEN_COLON); err != nil {
		return nil, err
	}
	p.consumeNewline()

	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	return &ForStmt{
		Pos:     p.makePos(tok),
		VarName: varTok.Literal,
		Iter:    iter,
		Body:    body,
	}, nil
}

func (p *Parser) parseFuncDef() (Stmt, error) {
	tok := p.advance() // consume 'def'

	nameTok := p.peek()
	if nameTok.Type != lexer.TOKEN_IDENT {
		return nil, fmt.Errorf("%d:%d: expected function name, got %s", nameTok.Line, nameTok.Col, nameTok.Type)
	}
	p.advance()

	if err := p.expect(lexer.TOKEN_LPAREN); err != nil {
		return nil, err
	}

	var params []FuncParam
	if p.peek().Type != lexer.TOKEN_RPAREN {
		for {
			pNameTok := p.peek()
			if pNameTok.Type != lexer.TOKEN_IDENT {
				return nil, fmt.Errorf("%d:%d: expected parameter name, got %s", pNameTok.Line, pNameTok.Col, pNameTok.Type)
			}
			p.advance()
			// Type annotation is optional (used for `self` in methods; the
			// checker enforces annotations on all other parameters).
			var typeAnn *TypeAnnotation
			if p.peek().Type == lexer.TOKEN_COLON {
				p.advance()
				ann, err := p.parseTypeAnnotation()
				if err != nil {
					return nil, err
				}
				typeAnn = ann
			}
			params = append(params, FuncParam{Name: pNameTok.Literal, TypeAnn: typeAnn})
			if p.peek().Type != lexer.TOKEN_COMMA {
				break
			}
			p.advance()
			if p.peek().Type == lexer.TOKEN_RPAREN {
				break // trailing comma
			}
		}
	}

	if err := p.expect(lexer.TOKEN_RPAREN); err != nil {
		return nil, err
	}

	// Return type
	var retType *TypeAnnotation
	if p.peek().Type == lexer.TOKEN_ARROW {
		p.advance()
		var err error
		retType, err = p.parseTypeAnnotation()
		if err != nil {
			return nil, err
		}
	}

	if err := p.expect(lexer.TOKEN_COLON); err != nil {
		return nil, err
	}

	// Inline stub body: `def foo(...): ...` on a single line. Used for @extern
	// declarations; also accepted for regular defs (body is empty, no-op).
	if p.peek().Type == lexer.TOKEN_ELLIPSIS {
		p.advance()
		p.consumeNewline()
		return &FuncDef{
			Pos:        p.makePos(tok),
			Name:       nameTok.Literal,
			Params:     params,
			ReturnType: retType,
			Body:       nil,
		}, nil
	}

	p.consumeNewline()

	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	return &FuncDef{
		Pos:        p.makePos(tok),
		Name:       nameTok.Literal,
		Params:     params,
		ReturnType: retType,
		Body:       body,
	}, nil
}

// parseDecoratedFuncDef handles a single-decorator function definition:
//
//	@extern
//	def name(...) -> T: ...
//
//	@extern("c_symbol")
//	def name(...) -> T: ...
//
// Only `@extern` is recognized; anything else is an error. Only one decorator
// is permitted per function in v1.
func (p *Parser) parseDecoratedFuncDef() (Stmt, error) {
	atTok := p.advance() // consume '@'
	nameTok := p.peek()
	if nameTok.Type != lexer.TOKEN_IDENT {
		return nil, fmt.Errorf("%d:%d: expected decorator name after '@', got %s", nameTok.Line, nameTok.Col, nameTok.Type)
	}
	p.advance()
	if nameTok.Literal != "extern" {
		return nil, fmt.Errorf("%d:%d: unknown decorator: @%s (only @extern is supported)", atTok.Line, atTok.Col, nameTok.Literal)
	}

	externSymbol := ""
	if p.peek().Type == lexer.TOKEN_LPAREN {
		p.advance() // consume (
		argTok := p.peek()
		if argTok.Type != lexer.TOKEN_STRING {
			return nil, fmt.Errorf("%d:%d: @extern argument must be a string literal, got %s", argTok.Line, argTok.Col, argTok.Type)
		}
		p.advance()
		externSymbol = argTok.Literal
		if err := p.expect(lexer.TOKEN_RPAREN); err != nil {
			return nil, err
		}
	}

	p.skipNewlines()

	if p.peek().Type != lexer.TOKEN_DEF {
		return nil, fmt.Errorf("%d:%d: expected 'def' after @extern decorator, got %s", p.peek().Line, p.peek().Col, p.peek().Type)
	}
	stmt, err := p.parseFuncDef()
	if err != nil {
		return nil, err
	}
	fd := stmt.(*FuncDef)
	fd.Extern = true
	fd.ExternSymbol = externSymbol
	return fd, nil
}

func (p *Parser) parseClassDef() (Stmt, error) {
	tok := p.advance() // consume 'class'

	nameTok := p.peek()
	if nameTok.Type != lexer.TOKEN_IDENT {
		return nil, fmt.Errorf("%d:%d: expected class name, got %s", nameTok.Line, nameTok.Col, nameTok.Type)
	}
	p.advance()

	base := ""
	if p.peek().Type == lexer.TOKEN_LPAREN {
		p.advance()
		if p.peek().Type != lexer.TOKEN_RPAREN {
			baseTok := p.peek()
			if baseTok.Type != lexer.TOKEN_IDENT {
				return nil, fmt.Errorf("%d:%d: expected base class name, got %s", baseTok.Line, baseTok.Col, baseTok.Type)
			}
			p.advance()
			base = baseTok.Literal
		}
		if err := p.expect(lexer.TOKEN_RPAREN); err != nil {
			return nil, err
		}
	}

	if err := p.expect(lexer.TOKEN_COLON); err != nil {
		return nil, err
	}
	p.consumeNewline()

	if err := p.expect(lexer.TOKEN_INDENT); err != nil {
		return nil, err
	}

	var methods []*FuncDef
	for p.peek().Type != lexer.TOKEN_DEDENT && !p.isAtEnd() {
		p.skipNewlines()
		if p.peek().Type == lexer.TOKEN_DEDENT || p.isAtEnd() {
			break
		}
		if p.peek().Type != lexer.TOKEN_DEF {
			return nil, fmt.Errorf("%d:%d: only method definitions are allowed inside a class body, got %s",
				p.peek().Line, p.peek().Col, p.peek().Type)
		}
		stmt, err := p.parseFuncDef()
		if err != nil {
			return nil, err
		}
		fd, ok := stmt.(*FuncDef)
		if !ok {
			return nil, fmt.Errorf("%d:%d: expected method definition", p.peek().Line, p.peek().Col)
		}
		methods = append(methods, fd)
	}

	if p.peek().Type == lexer.TOKEN_DEDENT {
		p.advance()
	}

	return &ClassDef{
		Pos:     p.makePos(tok),
		Name:    nameTok.Literal,
		Base:    base,
		Methods: methods,
	}, nil
}

func (p *Parser) parseTryStmt() (Stmt, error) {
	tok := p.advance() // consume 'try'
	if err := p.expect(lexer.TOKEN_COLON); err != nil {
		return nil, err
	}
	p.consumeNewline()
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	var excepts []ExceptClause
	sawBare := false
	for p.peek().Type == lexer.TOKEN_EXCEPT {
		if sawBare {
			return nil, fmt.Errorf("%d:%d: no except clauses may follow a bare `except:`",
				p.peek().Line, p.peek().Col)
		}
		excTok := p.advance() // consume 'except'
		clause := ExceptClause{Pos: p.makePos(excTok)}
		if p.peek().Type != lexer.TOKEN_COLON {
			ann, err := p.parseTypeAnnotation()
			if err != nil {
				return nil, err
			}
			clause.ExcType = ann
			if p.peek().Type == lexer.TOKEN_AS {
				p.advance()
				nameTok := p.peek()
				if nameTok.Type != lexer.TOKEN_IDENT {
					return nil, fmt.Errorf("%d:%d: expected identifier after 'as', got %s",
						nameTok.Line, nameTok.Col, nameTok.Type)
				}
				p.advance()
				clause.VarName = nameTok.Literal
			}
		} else {
			sawBare = true
		}
		if err := p.expect(lexer.TOKEN_COLON); err != nil {
			return nil, err
		}
		p.consumeNewline()
		clauseBody, err := p.parseBlock()
		if err != nil {
			return nil, err
		}
		clause.Body = clauseBody
		excepts = append(excepts, clause)
	}

	var finallyBody []Stmt
	hasFinally := false
	if p.peek().Type == lexer.TOKEN_FINALLY {
		p.advance()
		if err := p.expect(lexer.TOKEN_COLON); err != nil {
			return nil, err
		}
		p.consumeNewline()
		fb, err := p.parseBlock()
		if err != nil {
			return nil, err
		}
		finallyBody = fb
		hasFinally = true
	}

	if len(excepts) == 0 && !hasFinally {
		return nil, fmt.Errorf("%d:%d: try without except or finally",
			tok.Line, tok.Col)
	}

	return &TryStmt{
		Pos:         p.makePos(tok),
		Body:        body,
		Excepts:     excepts,
		FinallyBody: finallyBody,
		HasFinally:  hasFinally,
	}, nil
}

func (p *Parser) parseRaiseStmt() (Stmt, error) {
	tok := p.advance() // consume 'raise'
	if p.peek().Type == lexer.TOKEN_NEWLINE || p.peek().Type == lexer.TOKEN_EOF || p.peek().Type == lexer.TOKEN_DEDENT {
		return nil, fmt.Errorf("%d:%d: bare `raise` is not supported in v1; provide an exception instance",
			tok.Line, tok.Col)
	}
	value, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	p.consumeNewline()
	return &RaiseStmt{Pos: p.makePos(tok), Value: value}, nil
}

func (p *Parser) parseReturnStmt() (Stmt, error) {
	tok := p.advance() // consume 'return'

	var value Expr
	if p.peek().Type != lexer.TOKEN_NEWLINE && p.peek().Type != lexer.TOKEN_EOF && p.peek().Type != lexer.TOKEN_DEDENT {
		var err error
		value, err = p.parseExpr(0)
		if err != nil {
			return nil, err
		}
	}

	p.consumeNewline()
	return &ReturnStmt{Pos: p.makePos(tok), Value: value}, nil
}

func (p *Parser) parseImportStmt() (Stmt, error) {
	tok := p.advance() // consume 'import'
	nameTok := p.peek()
	if nameTok.Type != lexer.TOKEN_IDENT {
		return nil, fmt.Errorf("%d:%d: expected module name after 'import', got %s", nameTok.Line, nameTok.Col, nameTok.Type)
	}
	p.advance()
	if p.peek().Type == lexer.TOKEN_DOT {
		return nil, fmt.Errorf("%d:%d: dotted imports (e.g. pkg.sub) are not supported in v1", nameTok.Line, nameTok.Col)
	}

	alias := ""
	if p.peek().Type == lexer.TOKEN_AS {
		p.advance()
		aliasTok := p.peek()
		if aliasTok.Type != lexer.TOKEN_IDENT {
			return nil, fmt.Errorf("%d:%d: expected identifier after 'as', got %s", aliasTok.Line, aliasTok.Col, aliasTok.Type)
		}
		p.advance()
		alias = aliasTok.Literal
	}

	p.consumeNewline()
	return &ImportStmt{
		Pos:    p.makePos(tok),
		Module: nameTok.Literal,
		Alias:  alias,
	}, nil
}

func (p *Parser) parseFromImportStmt() (Stmt, error) {
	tok := p.advance() // consume 'from'
	modTok := p.peek()
	if modTok.Type != lexer.TOKEN_IDENT {
		return nil, fmt.Errorf("%d:%d: expected module name after 'from', got %s", modTok.Line, modTok.Col, modTok.Type)
	}
	p.advance()
	if p.peek().Type == lexer.TOKEN_DOT {
		return nil, fmt.Errorf("%d:%d: dotted imports (e.g. pkg.sub) are not supported in v1", modTok.Line, modTok.Col)
	}

	if p.peek().Type != lexer.TOKEN_IMPORT {
		return nil, fmt.Errorf("%d:%d: expected 'import' after module name, got %s", p.peek().Line, p.peek().Col, p.peek().Type)
	}
	p.advance()

	names := []ImportName{}
	for {
		nameTok := p.peek()
		if nameTok.Type != lexer.TOKEN_IDENT {
			return nil, fmt.Errorf("%d:%d: expected name after 'import', got %s", nameTok.Line, nameTok.Col, nameTok.Type)
		}
		p.advance()
		n := ImportName{Name: nameTok.Literal}
		if p.peek().Type == lexer.TOKEN_AS {
			p.advance()
			aliasTok := p.peek()
			if aliasTok.Type != lexer.TOKEN_IDENT {
				return nil, fmt.Errorf("%d:%d: expected identifier after 'as', got %s", aliasTok.Line, aliasTok.Col, aliasTok.Type)
			}
			p.advance()
			n.Alias = aliasTok.Literal
		}
		names = append(names, n)
		if p.peek().Type != lexer.TOKEN_COMMA {
			break
		}
		p.advance()
	}

	p.consumeNewline()
	return &FromImportStmt{
		Pos:    p.makePos(tok),
		Module: modTok.Literal,
		Names:  names,
	}, nil
}

func (p *Parser) parseBlock() ([]Stmt, error) {
	if err := p.expect(lexer.TOKEN_INDENT); err != nil {
		return nil, err
	}

	stmts := []Stmt{}
	for p.peek().Type != lexer.TOKEN_DEDENT && !p.isAtEnd() {
		p.skipNewlines()
		if p.peek().Type == lexer.TOKEN_DEDENT || p.isAtEnd() {
			break
		}
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		if stmt != nil {
			stmts = append(stmts, stmt)
		}
	}

	if p.peek().Type == lexer.TOKEN_DEDENT {
		p.advance()
	}

	return stmts, nil
}

// Helper methods

func (p *Parser) peek() lexer.Token {
	if p.current >= len(p.tokens) {
		return lexer.Token{Type: lexer.TOKEN_EOF}
	}
	return p.tokens[p.current]
}

func (p *Parser) peekN(n int) lexer.Token {
	idx := p.current + n
	if idx >= len(p.tokens) {
		return lexer.Token{Type: lexer.TOKEN_EOF}
	}
	return p.tokens[idx]
}

func (p *Parser) advance() lexer.Token {
	tok := p.peek()
	p.current++
	return tok
}

func (p *Parser) expect(t lexer.TokenType) error {
	tok := p.peek()
	if tok.Type != t {
		return fmt.Errorf("%d:%d: expected %s, got %s (%q)", tok.Line, tok.Col, t, tok.Type, tok.Literal)
	}
	p.advance()
	return nil
}

func (p *Parser) consumeNewline() {
	if p.peek().Type == lexer.TOKEN_NEWLINE {
		p.advance()
	}
}

func (p *Parser) skipNewlines() {
	for p.peek().Type == lexer.TOKEN_NEWLINE {
		p.advance()
	}
}

func (p *Parser) isAtEnd() bool {
	return p.current >= len(p.tokens) || p.tokens[p.current].Type == lexer.TOKEN_EOF
}

func (p *Parser) makePos(tok lexer.Token) Pos {
	return Pos{File: p.file, Line: tok.Line, Col: tok.Col}
}
