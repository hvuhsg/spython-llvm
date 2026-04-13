package parser

type Pos struct {
	File string
	Line int
	Col  int
}

// Type annotations
type TypeAnnotation struct {
	Pos    Pos
	Name   string
	Params []*TypeAnnotation // e.g., list[int] -> Params: [{Name: "int"}]
}

// Node interfaces
type Node interface {
	GetPos() Pos
}

type Stmt interface {
	Node
	stmtNode()
}

type Expr interface {
	Node
	exprNode()
	GetResolvedType() interface{}
	SetResolvedType(interface{})
}

// Base expression with resolved type
type baseExpr struct {
	resolvedType interface{}
}

func (b *baseExpr) GetResolvedType() interface{} { return b.resolvedType }
func (b *baseExpr) SetResolvedType(t interface{}) { b.resolvedType = t }

// Program is the root AST node
type Program struct {
	Stmts []Stmt
}

// Statements

type ExprStmt struct {
	Pos  Pos
	Expr Expr
}

func (s *ExprStmt) GetPos() Pos { return s.Pos }
func (s *ExprStmt) stmtNode()   {}

type AssignStmt struct {
	Pos     Pos
	Name    string
	TypeAnn *TypeAnnotation
	Value   Expr
}

func (s *AssignStmt) GetPos() Pos { return s.Pos }
func (s *AssignStmt) stmtNode()   {}

type AugAssignStmt struct {
	Pos  Pos
	Name string
	Op   string // "+", "-", "*", "/"
	Value Expr
}

func (s *AugAssignStmt) GetPos() Pos { return s.Pos }
func (s *AugAssignStmt) stmtNode()   {}

type IndexAssignStmt struct {
	Pos    Pos
	Object Expr
	Index  Expr
	Value  Expr
}

func (s *IndexAssignStmt) GetPos() Pos { return s.Pos }
func (s *IndexAssignStmt) stmtNode()   {}

// AttrAssignStmt represents attribute assignment: `obj.attr = value`.
type AttrAssignStmt struct {
	Pos    Pos
	Object Expr
	Attr   string
	Value  Expr
}

func (s *AttrAssignStmt) GetPos() Pos { return s.Pos }
func (s *AttrAssignStmt) stmtNode()   {}

type IfStmt struct {
	Pos       Pos
	Condition Expr
	Body      []Stmt
	Elifs     []ElifClause
	ElseBody  []Stmt
}

type ElifClause struct {
	Pos       Pos
	Condition Expr
	Body      []Stmt
}

func (s *IfStmt) GetPos() Pos { return s.Pos }
func (s *IfStmt) stmtNode()   {}

type WhileStmt struct {
	Pos       Pos
	Condition Expr
	Body      []Stmt
}

func (s *WhileStmt) GetPos() Pos { return s.Pos }
func (s *WhileStmt) stmtNode()   {}

type ForStmt struct {
	Pos      Pos
	VarName  string
	Iter     Expr // range call or collection expression
	Body     []Stmt
}

func (s *ForStmt) GetPos() Pos { return s.Pos }
func (s *ForStmt) stmtNode()   {}

type FuncDef struct {
	Pos        Pos
	Name       string
	Params     []FuncParam
	ReturnType *TypeAnnotation
	Body       []Stmt
}

type FuncParam struct {
	Name    string
	TypeAnn *TypeAnnotation
}

func (s *FuncDef) GetPos() Pos { return s.Pos }
func (s *FuncDef) stmtNode()   {}

type ReturnStmt struct {
	Pos   Pos
	Value Expr // nil for bare return
}

func (s *ReturnStmt) GetPos() Pos { return s.Pos }
func (s *ReturnStmt) stmtNode()   {}

type BreakStmt struct {
	Pos Pos
}

func (s *BreakStmt) GetPos() Pos { return s.Pos }
func (s *BreakStmt) stmtNode()   {}

type ContinueStmt struct {
	Pos Pos
}

func (s *ContinueStmt) GetPos() Pos { return s.Pos }
func (s *ContinueStmt) stmtNode()   {}

type ImportStmt struct {
	Pos    Pos
	Module string
	Alias  string
}

func (s *ImportStmt) GetPos() Pos { return s.Pos }
func (s *ImportStmt) stmtNode()   {}

type ImportName struct {
	Name  string
	Alias string
}

type FromImportStmt struct {
	Pos    Pos
	Module string
	Names  []ImportName
}

func (s *FromImportStmt) GetPos() Pos { return s.Pos }
func (s *FromImportStmt) stmtNode()   {}

type ClassDef struct {
	Pos     Pos
	Name    string
	Base    string // empty if no base class
	Methods []*FuncDef
}

func (s *ClassDef) GetPos() Pos { return s.Pos }
func (s *ClassDef) stmtNode()   {}

// Expressions

type IntLit struct {
	baseExpr
	Pos   Pos
	Value int64
}

func (e *IntLit) GetPos() Pos { return e.Pos }
func (e *IntLit) exprNode()   {}

type FloatLit struct {
	baseExpr
	Pos   Pos
	Value float64
}

func (e *FloatLit) GetPos() Pos { return e.Pos }
func (e *FloatLit) exprNode()   {}

type StrLit struct {
	baseExpr
	Pos   Pos
	Value string
}

func (e *StrLit) GetPos() Pos { return e.Pos }
func (e *StrLit) exprNode()   {}

type BoolLit struct {
	baseExpr
	Pos   Pos
	Value bool
}

func (e *BoolLit) GetPos() Pos { return e.Pos }
func (e *BoolLit) exprNode()   {}

type NoneLit struct {
	baseExpr
	Pos Pos
}

func (e *NoneLit) GetPos() Pos { return e.Pos }
func (e *NoneLit) exprNode()   {}

type IdentExpr struct {
	baseExpr
	Pos  Pos
	Name string
}

func (e *IdentExpr) GetPos() Pos { return e.Pos }
func (e *IdentExpr) exprNode()   {}

type BinaryExpr struct {
	baseExpr
	Pos   Pos
	Left  Expr
	Op    string
	Right Expr
}

func (e *BinaryExpr) GetPos() Pos { return e.Pos }
func (e *BinaryExpr) exprNode()   {}

type UnaryExpr struct {
	baseExpr
	Pos     Pos
	Op      string
	Operand Expr
}

func (e *UnaryExpr) GetPos() Pos { return e.Pos }
func (e *UnaryExpr) exprNode()   {}

type CallExpr struct {
	baseExpr
	Pos  Pos
	Func Expr
	Args []Expr
}

func (e *CallExpr) GetPos() Pos { return e.Pos }
func (e *CallExpr) exprNode()   {}

type IndexExpr struct {
	baseExpr
	Pos    Pos
	Object Expr
	Index  Expr
}

func (e *IndexExpr) GetPos() Pos { return e.Pos }
func (e *IndexExpr) exprNode()   {}

type AttrExpr struct {
	baseExpr
	Pos    Pos
	Object Expr
	Attr   string
}

func (e *AttrExpr) GetPos() Pos { return e.Pos }
func (e *AttrExpr) exprNode()   {}

type ListLit struct {
	baseExpr
	Pos      Pos
	Elements []Expr
}

func (e *ListLit) GetPos() Pos { return e.Pos }
func (e *ListLit) exprNode()   {}

type MapLit struct {
	baseExpr
	Pos    Pos
	Keys   []Expr
	Values []Expr
}

func (e *MapLit) GetPos() Pos { return e.Pos }
func (e *MapLit) exprNode()   {}

// SuperExpr represents a bare `super()` expression. It is only valid inside a
// method body and only as the object of an attribute-access, e.g.
// `super().foo(args)`. Using it anywhere else is a type error.
type SuperExpr struct {
	baseExpr
	Pos Pos
}

func (e *SuperExpr) GetPos() Pos { return e.Pos }
func (e *SuperExpr) exprNode()   {}
