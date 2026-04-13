package types

import (
	"fmt"

	"github.com/yehoyadashtinmetz/spython/parser"
)

type Checker struct {
	Env               *Env
	currentReturnType Type
	ModuleID          string // ID of the module being checked; set by loader
	imports           map[string]*ModuleType

	// Method-body context (populated only while checking a method's body).
	currentClass *ClassType // nil when not inside a class body
	classIDCtr   int        // monotonically increasing class id, starting at 1
}

func NewChecker() *Checker {
	c := &Checker{Env: NewEnv()}
	c.registerBuiltins()
	return c
}

func NewCheckerWithImports(moduleID string, imports map[string]*ModuleType) *Checker {
	c := &Checker{
		Env:      NewEnv(),
		ModuleID: moduleID,
		imports:  imports,
	}
	c.registerBuiltins()
	return c
}

func (c *Checker) registerBuiltins() {
	// print is special — handled in checkCallExpr
	// len is special — handled in checkCallExpr
	// range is special — handled in checkCallExpr
}

func (c *Checker) Check(program *parser.Program) error {
	if program == nil {
		return nil
	}

	// Collect names that will be defined locally at the top level so we can
	// detect collisions with imported names.
	localNames := map[string]bool{}
	for _, stmt := range program.Stmts {
		switch s := stmt.(type) {
		case *parser.FuncDef:
			localNames[s.Name] = true
		case *parser.ClassDef:
			localNames[s.Name] = true
		case *parser.AssignStmt:
			if s.TypeAnn != nil {
				localNames[s.Name] = true
			}
		}
	}

	// First pass: process imports so names are in scope before anything else
	for _, stmt := range program.Stmts {
		switch s := stmt.(type) {
		case *parser.ImportStmt:
			if err := c.processImportStmt(s, localNames); err != nil {
				return err
			}
		case *parser.FromImportStmt:
			if err := c.processFromImportStmt(s, localNames); err != nil {
				return err
			}
		}
	}

	// Second pass: register class types (name + base only; no fields/methods).
	// This lets method signatures reference class names in any order.
	for _, stmt := range program.Stmts {
		if cd, ok := stmt.(*parser.ClassDef); ok {
			if err := c.registerClassType(cd); err != nil {
				return err
			}
		}
	}

	// Third pass: register top-level function signatures.
	for _, stmt := range program.Stmts {
		if fd, ok := stmt.(*parser.FuncDef); ok {
			if err := c.registerFuncSignature(fd); err != nil {
				return err
			}
		}
	}

	// Fourth pass: for each class, register its method signatures (including
	// inherited methods copied from base). This must happen before field
	// inference because __init__ bodies may call methods on self.
	for _, stmt := range program.Stmts {
		if cd, ok := stmt.(*parser.ClassDef); ok {
			if err := c.registerClassMethods(cd); err != nil {
				return err
			}
		}
	}

	// Fifth pass: infer each class's fields by walking its __init__ body.
	for _, stmt := range program.Stmts {
		if cd, ok := stmt.(*parser.ClassDef); ok {
			if err := c.inferClassFields(cd); err != nil {
				return err
			}
		}
	}

	// Sixth pass: check everything else (imports/classes already processed for
	// registration; class method bodies are checked here too).
	for _, stmt := range program.Stmts {
		switch stmt.(type) {
		case *parser.ImportStmt, *parser.FromImportStmt:
			continue
		}
		if err := c.checkStmt(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (c *Checker) processImportStmt(s *parser.ImportStmt, localNames map[string]bool) error {
	mod, ok := c.imports[s.Module]
	if !ok {
		return fmt.Errorf("%d:%d: module not loaded: %s", s.Pos.Line, s.Pos.Col, s.Module)
	}
	name := s.Module
	if s.Alias != "" {
		name = s.Alias
	}
	if localNames[name] {
		return fmt.Errorf("%d:%d: name collision: %q is both imported and defined at top level", s.Pos.Line, s.Pos.Col, name)
	}
	c.Env.Define(name, mod)
	return nil
}

func (c *Checker) processFromImportStmt(s *parser.FromImportStmt, localNames map[string]bool) error {
	mod, ok := c.imports[s.Module]
	if !ok {
		return fmt.Errorf("%d:%d: module not loaded: %s", s.Pos.Line, s.Pos.Col, s.Module)
	}
	for _, n := range s.Names {
		t, ok := mod.Exports[n.Name]
		if !ok {
			return fmt.Errorf("%d:%d: %q is not exported from module %s", s.Pos.Line, s.Pos.Col, n.Name, s.Module)
		}
		effective := n.Name
		if n.Alias != "" {
			effective = n.Alias
		}
		if localNames[effective] {
			return fmt.Errorf("%d:%d: name collision: %q is both imported from %s and defined at top level", s.Pos.Line, s.Pos.Col, effective, s.Module)
		}
		c.Env.Define(effective, t)
	}
	return nil
}

// Exports returns the public surface of this module: top-level functions and
// top-level typed assignments. Must be called after Check() succeeds.
func (c *Checker) Exports(program *parser.Program) map[string]Type {
	out := map[string]Type{}
	if program == nil {
		return out
	}
	for _, stmt := range program.Stmts {
		switch s := stmt.(type) {
		case *parser.FuncDef:
			if t, ok := c.Env.Lookup(s.Name); ok {
				out[s.Name] = t
			}
		case *parser.AssignStmt:
			if s.TypeAnn != nil {
				if t, ok := c.Env.Lookup(s.Name); ok {
					out[s.Name] = t
				}
			}
		}
	}
	return out
}

func (c *Checker) registerFuncSignature(s *parser.FuncDef) error {
	paramTypes := []Type{}
	for _, p := range s.Params {
		if p.TypeAnn == nil {
			return fmt.Errorf("%d:%d: parameter %s requires a type annotation", s.Pos.Line, s.Pos.Col, p.Name)
		}
		pt := c.resolveTypeAnnotation(p.TypeAnn)
		if pt == nil {
			return fmt.Errorf("%d:%d: unknown parameter type: %s", s.Pos.Line, s.Pos.Col, p.TypeAnn.Name)
		}
		paramTypes = append(paramTypes, pt)
	}

	retType := Type(&NoneType{})
	if s.ReturnType != nil {
		retType = c.resolveTypeAnnotation(s.ReturnType)
		if retType == nil {
			return fmt.Errorf("%d:%d: unknown return type: %s", s.Pos.Line, s.Pos.Col, s.ReturnType.Name)
		}
	}

	funcType := &FuncType{Params: paramTypes, Return: retType, DefinedIn: c.ModuleID}
	c.Env.Define(s.Name, funcType)
	return nil
}

// registerClassType registers a class name and its base-class link. It does
// not register methods or fields yet.
func (c *Checker) registerClassType(s *parser.ClassDef) error {
	if _, exists := c.Env.LookupClass(s.Name); exists {
		return fmt.Errorf("%d:%d: class %s already defined", s.Pos.Line, s.Pos.Col, s.Name)
	}
	var base *ClassType
	if s.Base != "" {
		b, ok := c.Env.LookupClass(s.Base)
		if !ok {
			return fmt.Errorf("%d:%d: unknown base class: %s", s.Pos.Line, s.Pos.Col, s.Base)
		}
		base = b
	}
	c.classIDCtr++
	ct := &ClassType{
		Name:       s.Name,
		Base:       base,
		FieldIdx:   map[string]int{},
		Methods:    map[string]*FuncType{},
		OwnMethods: map[string]bool{},
		MethodSrc:  map[string]*ClassType{},
		ClassID:    c.classIDCtr,
		DefinedIn:  c.ModuleID,
	}
	c.Env.DefineClass(s.Name, ct)
	// Also expose the class name as a value so it can appear in expressions
	// (e.g., Circle(1.0), isinstance(x, Circle)).
	c.Env.Define(s.Name, ct)
	return nil
}

// registerClassMethods resolves method signatures for each method on the class,
// inherits base methods, and rejects override-signature mismatches.
func (c *Checker) registerClassMethods(s *parser.ClassDef) error {
	ct, ok := c.Env.LookupClass(s.Name)
	if !ok {
		return fmt.Errorf("%d:%d: internal: class %s not registered", s.Pos.Line, s.Pos.Col, s.Name)
	}

	// Start with base methods.
	if ct.Base != nil {
		for name, sig := range ct.Base.Methods {
			ct.Methods[name] = sig
			ct.MethodSrc[name] = ct.Base.MethodSrc[name]
		}
	}

	for _, m := range s.Methods {
		if len(m.Params) == 0 || m.Params[0].Name != "self" {
			return fmt.Errorf("%d:%d: method %s.%s must have 'self' as its first parameter",
				m.Pos.Line, m.Pos.Col, s.Name, m.Name)
		}
		if m.Params[0].TypeAnn != nil {
			return fmt.Errorf("%d:%d: method %s.%s: 'self' must not have a type annotation",
				m.Pos.Line, m.Pos.Col, s.Name, m.Name)
		}
		// Resolve method signature (self-less: self is implied).
		paramTypes := []Type{}
		for i := 1; i < len(m.Params); i++ {
			p := m.Params[i]
			if p.TypeAnn == nil {
				return fmt.Errorf("%d:%d: method %s.%s: parameter %s needs a type annotation",
					m.Pos.Line, m.Pos.Col, s.Name, m.Name, p.Name)
			}
			pt := c.resolveTypeAnnotation(p.TypeAnn)
			if pt == nil {
				return fmt.Errorf("%d:%d: method %s.%s: unknown parameter type %s",
					m.Pos.Line, m.Pos.Col, s.Name, m.Name, p.TypeAnn.Name)
			}
			paramTypes = append(paramTypes, pt)
		}
		retType := Type(&NoneType{})
		if m.ReturnType != nil {
			retType = c.resolveTypeAnnotation(m.ReturnType)
			if retType == nil {
				return fmt.Errorf("%d:%d: method %s.%s: unknown return type %s",
					m.Pos.Line, m.Pos.Col, s.Name, m.Name, m.ReturnType.Name)
			}
		}
		sig := &FuncType{Params: paramTypes, Return: retType, DefinedIn: c.ModuleID}

		// If overriding, enforce strict signature equality with the base.
		// __init__ is exempt because Python-style constructors commonly vary
		// their own signature across the class hierarchy.
		if m.Name != "__init__" {
			if existing, ok := ct.Methods[m.Name]; ok {
				if !existing.Equals(sig) {
					return fmt.Errorf("%d:%d: method %s.%s overrides %s with mismatched signature: expected %s, got %s",
						m.Pos.Line, m.Pos.Col, s.Name, m.Name, ct.MethodSrc[m.Name].Name, existing, sig)
				}
			}
		}

		ct.Methods[m.Name] = sig
		ct.OwnMethods[m.Name] = true
		ct.MethodSrc[m.Name] = ct
	}

	return nil
}

// inferClassFields walks __init__ (if present) and infers instance fields from
// `self.x = <expr>` assignments. Child classes inherit base fields first;
// `super().__init__(...)` does not need to be parsed specially because base
// fields are already present on the layout.
func (c *Checker) inferClassFields(s *parser.ClassDef) error {
	ct, ok := c.Env.LookupClass(s.Name)
	if !ok {
		return fmt.Errorf("%d:%d: internal: class %s not registered", s.Pos.Line, s.Pos.Col, s.Name)
	}

	// Inherit base fields first so the layout starts with parent's layout.
	if ct.Base != nil {
		for _, f := range ct.Base.Fields {
			ct.FieldIdx[f.Name] = len(ct.Fields)
			ct.Fields = append(ct.Fields, f)
		}
	}

	var init *parser.FuncDef
	for _, m := range s.Methods {
		if m.Name == "__init__" {
			init = m
			break
		}
	}
	if init == nil {
		return nil
	}

	// Type-check the body in a scratch env with `self` in scope.
	instType := &InstanceType{Class: ct}
	prevRet := c.currentReturnType
	prevClass := c.currentClass
	c.currentReturnType = &NoneType{}
	c.currentClass = ct
	c.Env.Push()
	c.Env.Define("self", instType)
	for i := 1; i < len(init.Params); i++ {
		p := init.Params[i]
		pt := c.resolveTypeAnnotation(p.TypeAnn)
		if pt == nil {
			c.Env.Pop()
			c.currentClass = prevClass
			c.currentReturnType = prevRet
			return fmt.Errorf("%d:%d: unknown type %s", p.TypeAnn.Pos.Line, p.TypeAnn.Pos.Col, p.TypeAnn.Name)
		}
		c.Env.Define(p.Name, pt)
	}

	if err := c.inferFieldsFromStmts(ct, init.Body); err != nil {
		c.Env.Pop()
		c.currentClass = prevClass
		c.currentReturnType = prevRet
		return err
	}
	c.Env.Pop()
	c.currentClass = prevClass
	c.currentReturnType = prevRet
	return nil
}

// inferFieldsFromStmts walks a block, collecting `self.x = <expr>` patterns.
// It type-checks RHS expressions using the full expression machinery so
// references to already-inferred fields and to methods work naturally.
func (c *Checker) inferFieldsFromStmts(ct *ClassType, stmts []parser.Stmt) error {
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *parser.AttrAssignStmt:
			ident, ok := s.Object.(*parser.IdentExpr)
			if !ok || ident.Name != "self" {
				// Non-self attribute assignment inside __init__: still check RHS normally.
				if _, err := c.checkExpr(s.Value); err != nil {
					return err
				}
				continue
			}
			valType, err := c.checkExpr(s.Value)
			if err != nil {
				return err
			}
			if idx, already := ct.FieldIdx[s.Attr]; already {
				if !ct.Fields[idx].Type.Equals(valType) {
					return fmt.Errorf("%d:%d: field %s.%s already has type %s; cannot reassign %s",
						s.Pos.Line, s.Pos.Col, ct.Name, s.Attr, ct.Fields[idx].Type, valType)
				}
				s.Object.SetResolvedType(&InstanceType{Class: ct})
				continue
			}
			ct.FieldIdx[s.Attr] = len(ct.Fields)
			ct.Fields = append(ct.Fields, ClassField{Name: s.Attr, Type: valType})
			s.Object.SetResolvedType(&InstanceType{Class: ct})
		case *parser.IfStmt:
			if _, err := c.checkExpr(s.Condition); err != nil {
				return err
			}
			if err := c.inferFieldsFromStmts(ct, s.Body); err != nil {
				return err
			}
			for _, elif := range s.Elifs {
				if _, err := c.checkExpr(elif.Condition); err != nil {
					return err
				}
				if err := c.inferFieldsFromStmts(ct, elif.Body); err != nil {
					return err
				}
			}
			if err := c.inferFieldsFromStmts(ct, s.ElseBody); err != nil {
				return err
			}
		case *parser.WhileStmt:
			if _, err := c.checkExpr(s.Condition); err != nil {
				return err
			}
			if err := c.inferFieldsFromStmts(ct, s.Body); err != nil {
				return err
			}
		case *parser.ForStmt:
			// Rarely useful in __init__; still support non-self assignments.
			if err := c.checkForStmt(s); err != nil {
				return err
			}
		case *parser.ReturnStmt:
			// Bare return ok; explicit return value disallowed from __init__.
			if s.Value != nil {
				return fmt.Errorf("%d:%d: __init__ must not return a value", s.Pos.Line, s.Pos.Col)
			}
		default:
			if _, ok := stmt.(*parser.AttrAssignStmt); ok {
				continue
			}
			if err := c.checkStmt(stmt); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Checker) checkStmt(stmt parser.Stmt) error {
	switch s := stmt.(type) {
	case *parser.ExprStmt:
		_, err := c.checkExpr(s.Expr)
		return err
	case *parser.AssignStmt:
		return c.checkAssignStmt(s)
	case *parser.AugAssignStmt:
		return c.checkAugAssignStmt(s)
	case *parser.IndexAssignStmt:
		return c.checkIndexAssignStmt(s)
	case *parser.AttrAssignStmt:
		return c.checkAttrAssignStmt(s)
	case *parser.IfStmt:
		return c.checkIfStmt(s)
	case *parser.WhileStmt:
		return c.checkWhileStmt(s)
	case *parser.ForStmt:
		return c.checkForStmt(s)
	case *parser.FuncDef:
		return c.checkFuncDef(s)
	case *parser.ClassDef:
		return c.checkClassDef(s)
	case *parser.ReturnStmt:
		return c.checkReturnStmt(s)
	case *parser.BreakStmt, *parser.ContinueStmt:
		return nil
	default:
		return fmt.Errorf("unknown statement type: %T", stmt)
	}
}

func (c *Checker) checkAttrAssignStmt(s *parser.AttrAssignStmt) error {
	objType, err := c.checkExpr(s.Object)
	if err != nil {
		return err
	}
	valType, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}
	inst, ok := objType.(*InstanceType)
	if !ok {
		return fmt.Errorf("%d:%d: cannot set attribute on %s", s.Pos.Line, s.Pos.Col, objType)
	}
	idx, ok := inst.Class.FieldIdx[s.Attr]
	if !ok {
		return fmt.Errorf("%d:%d: %s has no field %s", s.Pos.Line, s.Pos.Col, inst.Class.Name, s.Attr)
	}
	field := inst.Class.Fields[idx]
	if !IsAssignable(valType, field.Type) {
		return fmt.Errorf("%d:%d: cannot assign %s to field %s.%s of type %s",
			s.Pos.Line, s.Pos.Col, valType, inst.Class.Name, s.Attr, field.Type)
	}
	return nil
}

func (c *Checker) checkClassDef(s *parser.ClassDef) error {
	ct, ok := c.Env.LookupClass(s.Name)
	if !ok {
		return fmt.Errorf("%d:%d: internal: class %s not registered", s.Pos.Line, s.Pos.Col, s.Name)
	}
	// Check each method body with `self` in scope and the method's signature.
	for _, m := range s.Methods {
		if err := c.checkMethodBody(ct, m); err != nil {
			return err
		}
	}
	return nil
}

func (c *Checker) checkMethodBody(ct *ClassType, m *parser.FuncDef) error {
	sig, ok := ct.Methods[m.Name]
	if !ok {
		return fmt.Errorf("%d:%d: internal: method %s.%s not registered", m.Pos.Line, m.Pos.Col, ct.Name, m.Name)
	}
	prevRet := c.currentReturnType
	prevClass := c.currentClass
	c.currentReturnType = sig.Return
	c.currentClass = ct

	c.Env.Push()
	c.Env.Define("self", &InstanceType{Class: ct})
	for i := 1; i < len(m.Params); i++ {
		c.Env.Define(m.Params[i].Name, sig.Params[i-1])
	}
	defer func() {
		c.Env.Pop()
		c.currentClass = prevClass
		c.currentReturnType = prevRet
	}()

	for _, stmt := range m.Body {
		if err := c.checkStmt(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (c *Checker) checkAssignStmt(s *parser.AssignStmt) error {
	valType, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}

	if s.TypeAnn != nil {
		annType := c.resolveTypeAnnotation(s.TypeAnn)
		if annType == nil {
			return fmt.Errorf("%d:%d: unknown type: %s", s.TypeAnn.Pos.Line, s.TypeAnn.Pos.Col, s.TypeAnn.Name)
		}
		if !IsAssignable(valType, annType) {
			return fmt.Errorf("%d:%d: cannot assign %s to %s", s.Pos.Line, s.Pos.Col, valType, annType)
		}
		c.Env.Define(s.Name, annType)
	} else {
		// Reassignment without type annotation
		existing, ok := c.Env.Lookup(s.Name)
		if !ok {
			return fmt.Errorf("%d:%d: undefined variable: %s", s.Pos.Line, s.Pos.Col, s.Name)
		}
		if !IsAssignable(valType, existing) {
			return fmt.Errorf("%d:%d: cannot assign %s to %s", s.Pos.Line, s.Pos.Col, valType, existing)
		}
	}

	return nil
}

func (c *Checker) checkAugAssignStmt(s *parser.AugAssignStmt) error {
	varType, ok := c.Env.Lookup(s.Name)
	if !ok {
		return fmt.Errorf("%d:%d: undefined variable: %s", s.Pos.Line, s.Pos.Col, s.Name)
	}

	valType, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}

	if !varType.Equals(valType) {
		return fmt.Errorf("%d:%d: cannot use %s with %s in augmented assignment", s.Pos.Line, s.Pos.Col, valType, varType)
	}

	return nil
}

func (c *Checker) checkIndexAssignStmt(s *parser.IndexAssignStmt) error {
	objType, err := c.checkExpr(s.Object)
	if err != nil {
		return err
	}
	idxType, err := c.checkExpr(s.Index)
	if err != nil {
		return err
	}
	valType, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}

	switch t := objType.(type) {
	case *ListType:
		if _, ok := idxType.(*IntType); !ok {
			return fmt.Errorf("%d:%d: list index must be int, got %s", s.Pos.Line, s.Pos.Col, idxType)
		}
		if !t.Elem.Equals(valType) {
			return fmt.Errorf("%d:%d: cannot assign %s to list[%s]", s.Pos.Line, s.Pos.Col, valType, t.Elem)
		}
	case *MapType:
		if !t.Key.Equals(idxType) {
			return fmt.Errorf("%d:%d: map key type mismatch: expected %s, got %s", s.Pos.Line, s.Pos.Col, t.Key, idxType)
		}
		if !t.Value.Equals(valType) {
			return fmt.Errorf("%d:%d: cannot assign %s to map value type %s", s.Pos.Line, s.Pos.Col, valType, t.Value)
		}
	default:
		return fmt.Errorf("%d:%d: cannot index-assign to %s", s.Pos.Line, s.Pos.Col, objType)
	}

	return nil
}

func (c *Checker) checkIfStmt(s *parser.IfStmt) error {
	condType, err := c.checkExpr(s.Condition)
	if err != nil {
		return err
	}
	if _, ok := condType.(*BoolType); !ok {
		return fmt.Errorf("%d:%d: if condition must be bool, got %s", s.Pos.Line, s.Pos.Col, condType)
	}

	c.Env.Push()
	for _, stmt := range s.Body {
		if err := c.checkStmt(stmt); err != nil {
			return err
		}
	}
	c.Env.Pop()

	for _, elif := range s.Elifs {
		elifType, err := c.checkExpr(elif.Condition)
		if err != nil {
			return err
		}
		if _, ok := elifType.(*BoolType); !ok {
			return fmt.Errorf("%d:%d: elif condition must be bool, got %s", elif.Pos.Line, elif.Pos.Col, elifType)
		}
		c.Env.Push()
		for _, stmt := range elif.Body {
			if err := c.checkStmt(stmt); err != nil {
				return err
			}
		}
		c.Env.Pop()
	}

	if s.ElseBody != nil {
		c.Env.Push()
		for _, stmt := range s.ElseBody {
			if err := c.checkStmt(stmt); err != nil {
				return err
			}
		}
		c.Env.Pop()
	}

	return nil
}

func (c *Checker) checkWhileStmt(s *parser.WhileStmt) error {
	condType, err := c.checkExpr(s.Condition)
	if err != nil {
		return err
	}
	if _, ok := condType.(*BoolType); !ok {
		return fmt.Errorf("%d:%d: while condition must be bool, got %s", s.Pos.Line, s.Pos.Col, condType)
	}

	c.Env.Push()
	for _, stmt := range s.Body {
		if err := c.checkStmt(stmt); err != nil {
			return err
		}
	}
	c.Env.Pop()

	return nil
}

func (c *Checker) checkForStmt(s *parser.ForStmt) error {
	iterType, err := c.checkExpr(s.Iter)
	if err != nil {
		return err
	}

	c.Env.Push()

	// Determine loop variable type from iterator
	switch t := iterType.(type) {
	case *IntType:
		// range() returns int (special case, range call resolves to IntType)
		c.Env.Define(s.VarName, &IntType{})
	case *ListType:
		c.Env.Define(s.VarName, t.Elem)
	case *StrType:
		c.Env.Define(s.VarName, &StrType{})
	default:
		return fmt.Errorf("%d:%d: cannot iterate over %s", s.Pos.Line, s.Pos.Col, iterType)
	}

	for _, stmt := range s.Body {
		if err := c.checkStmt(stmt); err != nil {
			return err
		}
	}
	c.Env.Pop()

	return nil
}

func (c *Checker) checkFuncDef(s *parser.FuncDef) error {
	// Build function type
	paramTypes := []Type{}
	for _, p := range s.Params {
		if p.TypeAnn == nil {
			return fmt.Errorf("%d:%d: parameter %s requires a type annotation", s.Pos.Line, s.Pos.Col, p.Name)
		}
		pt := c.resolveTypeAnnotation(p.TypeAnn)
		if pt == nil {
			return fmt.Errorf("%d:%d: unknown parameter type: %s", s.Pos.Line, s.Pos.Col, p.TypeAnn.Name)
		}
		paramTypes = append(paramTypes, pt)
	}

	retType := Type(&NoneType{})
	if s.ReturnType != nil {
		retType = c.resolveTypeAnnotation(s.ReturnType)
		if retType == nil {
			return fmt.Errorf("%d:%d: unknown return type: %s", s.Pos.Line, s.Pos.Col, s.ReturnType.Name)
		}
	}

	funcType := &FuncType{Params: paramTypes, Return: retType, DefinedIn: c.ModuleID}
	c.Env.Define(s.Name, funcType)

	// Check body in new scope
	prevReturnType := c.currentReturnType
	c.currentReturnType = retType
	c.Env.Push()
	for i, p := range s.Params {
		c.Env.Define(p.Name, paramTypes[i])
	}
	for _, stmt := range s.Body {
		if err := c.checkStmt(stmt); err != nil {
			return err
		}
	}
	c.Env.Pop()
	c.currentReturnType = prevReturnType

	return nil
}

func (c *Checker) checkReturnStmt(s *parser.ReturnStmt) error {
	if s.Value == nil {
		if c.currentReturnType != nil {
			if _, ok := c.currentReturnType.(*NoneType); !ok {
				return fmt.Errorf("%d:%d: expected return value of type %s", s.Pos.Line, s.Pos.Col, c.currentReturnType)
			}
		}
		return nil
	}

	valType, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}

	if c.currentReturnType != nil && !IsAssignable(valType, c.currentReturnType) {
		return fmt.Errorf("%d:%d: return type mismatch: expected %s, got %s", s.Pos.Line, s.Pos.Col, c.currentReturnType, valType)
	}

	return nil
}

func (c *Checker) checkExpr(expr parser.Expr) (Type, error) {
	var t Type
	var err error

	switch e := expr.(type) {
	case *parser.IntLit:
		t = &IntType{}
	case *parser.FloatLit:
		t = &FloatType{}
	case *parser.StrLit:
		t = &StrType{}
	case *parser.BoolLit:
		t = &BoolType{}
	case *parser.NoneLit:
		t = &NoneType{}
	case *parser.IdentExpr:
		t, err = c.checkIdentExpr(e)
	case *parser.BinaryExpr:
		t, err = c.checkBinaryExpr(e)
	case *parser.UnaryExpr:
		t, err = c.checkUnaryExpr(e)
	case *parser.CallExpr:
		t, err = c.checkCallExpr(e)
	case *parser.IndexExpr:
		t, err = c.checkIndexExpr(e)
	case *parser.AttrExpr:
		t, err = c.checkAttrExpr(e)
	case *parser.ListLit:
		t, err = c.checkListLit(e)
	case *parser.MapLit:
		t, err = c.checkMapLit(e)
	default:
		return nil, fmt.Errorf("unknown expression type: %T", expr)
	}

	if err != nil {
		return nil, err
	}
	expr.SetResolvedType(t)
	return t, nil
}

func (c *Checker) checkIdentExpr(e *parser.IdentExpr) (Type, error) {
	t, ok := c.Env.Lookup(e.Name)
	if !ok {
		return nil, fmt.Errorf("%d:%d: undefined variable: %s", e.Pos.Line, e.Pos.Col, e.Name)
	}
	return t, nil
}

func (c *Checker) checkBinaryExpr(e *parser.BinaryExpr) (Type, error) {
	leftType, err := c.checkExpr(e.Left)
	if err != nil {
		return nil, err
	}
	rightType, err := c.checkExpr(e.Right)
	if err != nil {
		return nil, err
	}

	// Operator overloading: if left operand is a class instance, dispatch to
	// the appropriate dunder method.
	if inst, ok := leftType.(*InstanceType); ok {
		dunder := binaryDunderName(e.Op)
		if dunder == "" {
			return nil, fmt.Errorf("%d:%d: operator %s not supported on %s", e.Pos.Line, e.Pos.Col, e.Op, inst.Class.Name)
		}
		sig, ok := inst.Class.Methods[dunder]
		if !ok {
			return nil, fmt.Errorf("%d:%d: %s does not define %s for %s",
				e.Pos.Line, e.Pos.Col, inst.Class.Name, dunder, e.Op)
		}
		if len(sig.Params) != 1 {
			return nil, fmt.Errorf("%d:%d: %s.%s must take exactly one argument (besides self)",
				e.Pos.Line, e.Pos.Col, inst.Class.Name, dunder)
		}
		if !IsAssignable(rightType, sig.Params[0]) {
			return nil, fmt.Errorf("%d:%d: %s.%s expects %s, got %s",
				e.Pos.Line, e.Pos.Col, inst.Class.Name, dunder, sig.Params[0], rightType)
		}
		return sig.Return, nil
	}

	switch e.Op {
	case "+":
		// int+int, float+float, str+str
		if leftType.Equals(rightType) {
			switch leftType.(type) {
			case *IntType, *FloatType, *StrType:
				return leftType, nil
			}
		}
		return nil, fmt.Errorf("%d:%d: cannot add %s and %s", e.Pos.Line, e.Pos.Col, leftType, rightType)

	case "-", "*", "/", "//", "%":
		if leftType.Equals(rightType) {
			switch leftType.(type) {
			case *IntType, *FloatType:
				return leftType, nil
			}
		}
		return nil, fmt.Errorf("%d:%d: cannot use %s on %s and %s", e.Pos.Line, e.Pos.Col, e.Op, leftType, rightType)

	case "**":
		if leftType.Equals(rightType) {
			switch leftType.(type) {
			case *IntType, *FloatType:
				return leftType, nil
			}
		}
		return nil, fmt.Errorf("%d:%d: cannot use ** on %s and %s", e.Pos.Line, e.Pos.Col, leftType, rightType)

	case "==", "!=":
		if leftType.Equals(rightType) {
			return &BoolType{}, nil
		}
		return nil, fmt.Errorf("%d:%d: cannot compare %s and %s", e.Pos.Line, e.Pos.Col, leftType, rightType)

	case "<", ">", "<=", ">=":
		if leftType.Equals(rightType) {
			switch leftType.(type) {
			case *IntType, *FloatType, *StrType:
				return &BoolType{}, nil
			}
		}
		return nil, fmt.Errorf("%d:%d: cannot compare %s and %s with %s", e.Pos.Line, e.Pos.Col, leftType, rightType, e.Op)

	case "and", "or":
		if _, ok := leftType.(*BoolType); !ok {
			return nil, fmt.Errorf("%d:%d: %s requires bool operands, got %s", e.Pos.Line, e.Pos.Col, e.Op, leftType)
		}
		if _, ok := rightType.(*BoolType); !ok {
			return nil, fmt.Errorf("%d:%d: %s requires bool operands, got %s", e.Pos.Line, e.Pos.Col, e.Op, rightType)
		}
		return &BoolType{}, nil

	case "&", "|", "^", "<<", ">>":
		if _, ok := leftType.(*IntType); !ok {
			return nil, fmt.Errorf("%d:%d: bitwise %s requires int operands, got %s", e.Pos.Line, e.Pos.Col, e.Op, leftType)
		}
		if _, ok := rightType.(*IntType); !ok {
			return nil, fmt.Errorf("%d:%d: bitwise %s requires int operands, got %s", e.Pos.Line, e.Pos.Col, e.Op, rightType)
		}
		return &IntType{}, nil
	}

	return nil, fmt.Errorf("%d:%d: unknown binary operator: %s", e.Pos.Line, e.Pos.Col, e.Op)
}

func (c *Checker) checkUnaryExpr(e *parser.UnaryExpr) (Type, error) {
	operandType, err := c.checkExpr(e.Operand)
	if err != nil {
		return nil, err
	}

	// Unary minus on a class instance → __neg__.
	if e.Op == "-" {
		if inst, ok := operandType.(*InstanceType); ok {
			sig, ok := inst.Class.Methods["__neg__"]
			if !ok {
				return nil, fmt.Errorf("%d:%d: %s does not define __neg__", e.Pos.Line, e.Pos.Col, inst.Class.Name)
			}
			if len(sig.Params) != 0 {
				return nil, fmt.Errorf("%d:%d: %s.__neg__ must take no arguments (besides self)",
					e.Pos.Line, e.Pos.Col, inst.Class.Name)
			}
			return sig.Return, nil
		}
	}

	switch e.Op {
	case "-":
		switch operandType.(type) {
		case *IntType, *FloatType:
			return operandType, nil
		}
		return nil, fmt.Errorf("%d:%d: cannot negate %s", e.Pos.Line, e.Pos.Col, operandType)
	case "not":
		if _, ok := operandType.(*BoolType); !ok {
			return nil, fmt.Errorf("%d:%d: not requires bool, got %s", e.Pos.Line, e.Pos.Col, operandType)
		}
		return &BoolType{}, nil
	case "~":
		if _, ok := operandType.(*IntType); !ok {
			return nil, fmt.Errorf("%d:%d: ~ requires int, got %s", e.Pos.Line, e.Pos.Col, operandType)
		}
		return &IntType{}, nil
	}

	return nil, fmt.Errorf("%d:%d: unknown unary operator: %s", e.Pos.Line, e.Pos.Col, e.Op)
}

func (c *Checker) checkCallExpr(e *parser.CallExpr) (Type, error) {
	// Handle built-in functions
	if ident, ok := e.Func.(*parser.IdentExpr); ok {
		switch ident.Name {
		case "isinstance":
			if len(e.Args) != 2 {
				return nil, fmt.Errorf("%d:%d: isinstance() takes exactly 2 arguments", e.Pos.Line, e.Pos.Col)
			}
			objType, err := c.checkExpr(e.Args[0])
			if err != nil {
				return nil, err
			}
			if _, ok := objType.(*InstanceType); !ok {
				return nil, fmt.Errorf("%d:%d: first argument to isinstance must be a class instance, got %s",
					e.Pos.Line, e.Pos.Col, objType)
			}
			clsIdent, ok := e.Args[1].(*parser.IdentExpr)
			if !ok {
				return nil, fmt.Errorf("%d:%d: second argument to isinstance must be a class name", e.Pos.Line, e.Pos.Col)
			}
			ct, ok := c.Env.LookupClass(clsIdent.Name)
			if !ok {
				return nil, fmt.Errorf("%d:%d: unknown class: %s", e.Pos.Line, e.Pos.Col, clsIdent.Name)
			}
			// Record the class type on the expr so codegen knows which class_id to match.
			e.Args[1].SetResolvedType(ct)
			return &BoolType{}, nil
		case "print":
			// print accepts any number of any-typed args
			for _, arg := range e.Args {
				_, err := c.checkExpr(arg)
				if err != nil {
					return nil, err
				}
			}
			return &NoneType{}, nil

		case "len":
			if len(e.Args) != 1 {
				return nil, fmt.Errorf("%d:%d: len() takes exactly 1 argument", e.Pos.Line, e.Pos.Col)
			}
			argType, err := c.checkExpr(e.Args[0])
			if err != nil {
				return nil, err
			}
			switch argType.(type) {
			case *StrType, *ListType, *MapType:
				return &IntType{}, nil
			}
			return nil, fmt.Errorf("%d:%d: len() not supported for %s", e.Pos.Line, e.Pos.Col, argType)

		case "range":
			if len(e.Args) < 1 || len(e.Args) > 3 {
				return nil, fmt.Errorf("%d:%d: range() takes 1 to 3 arguments", e.Pos.Line, e.Pos.Col)
			}
			for _, arg := range e.Args {
				argType, err := c.checkExpr(arg)
				if err != nil {
					return nil, err
				}
				if _, ok := argType.(*IntType); !ok {
					return nil, fmt.Errorf("%d:%d: range() arguments must be int, got %s", e.Pos.Line, e.Pos.Col, argType)
				}
			}
			// range returns an iterator that yields ints
			// For type checking purposes, we treat it as IntType (used by for loops)
			return &IntType{}, nil

		case "int":
			if len(e.Args) != 1 {
				return nil, fmt.Errorf("%d:%d: int() takes exactly 1 argument", e.Pos.Line, e.Pos.Col)
			}
			argType, err := c.checkExpr(e.Args[0])
			if err != nil {
				return nil, err
			}
			switch argType.(type) {
			case *IntType, *FloatType, *StrType, *BoolType:
				return &IntType{}, nil
			}
			return nil, fmt.Errorf("%d:%d: cannot convert %s to int", e.Pos.Line, e.Pos.Col, argType)

		case "float":
			if len(e.Args) != 1 {
				return nil, fmt.Errorf("%d:%d: float() takes exactly 1 argument", e.Pos.Line, e.Pos.Col)
			}
			argType, err := c.checkExpr(e.Args[0])
			if err != nil {
				return nil, err
			}
			switch argType.(type) {
			case *IntType, *FloatType, *StrType:
				return &FloatType{}, nil
			}
			return nil, fmt.Errorf("%d:%d: cannot convert %s to float", e.Pos.Line, e.Pos.Col, argType)

		case "str":
			if len(e.Args) != 1 {
				return nil, fmt.Errorf("%d:%d: str() takes exactly 1 argument", e.Pos.Line, e.Pos.Col)
			}
			_, err := c.checkExpr(e.Args[0])
			if err != nil {
				return nil, err
			}
			return &StrType{}, nil
		}
	}

	// User-defined function call (or constructor, or method)
	funcExpr, err := c.checkExpr(e.Func)
	if err != nil {
		return nil, err
	}

	// Constructor call: ClassName(args)
	if ct, ok := funcExpr.(*ClassType); ok {
		initSig := findInitSig(ct)
		var params []Type
		if initSig != nil {
			params = initSig.Params
		}
		if len(e.Args) != len(params) {
			return nil, fmt.Errorf("%d:%d: %s constructor expected %d arguments, got %d",
				e.Pos.Line, e.Pos.Col, ct.Name, len(params), len(e.Args))
		}
		for i, arg := range e.Args {
			argType, err := c.checkExpr(arg)
			if err != nil {
				return nil, err
			}
			if !IsAssignable(argType, params[i]) {
				return nil, fmt.Errorf("%d:%d: %s constructor argument %d: expected %s, got %s",
					e.Pos.Line, e.Pos.Col, ct.Name, i+1, params[i], argType)
			}
		}
		return &InstanceType{Class: ct}, nil
	}

	funcType, ok := funcExpr.(*FuncType)
	if !ok {
		return nil, fmt.Errorf("%d:%d: %s is not callable", e.Pos.Line, e.Pos.Col, funcExpr)
	}

	if len(e.Args) != len(funcType.Params) {
		return nil, fmt.Errorf("%d:%d: expected %d arguments, got %d", e.Pos.Line, e.Pos.Col, len(funcType.Params), len(e.Args))
	}

	for i, arg := range e.Args {
		argType, err := c.checkExpr(arg)
		if err != nil {
			return nil, err
		}
		if !IsAssignable(argType, funcType.Params[i]) {
			return nil, fmt.Errorf("%d:%d: argument %d: expected %s, got %s", e.Pos.Line, e.Pos.Col, i+1, funcType.Params[i], argType)
		}
	}

	return funcType.Return, nil
}

func (c *Checker) checkIndexExpr(e *parser.IndexExpr) (Type, error) {
	objType, err := c.checkExpr(e.Object)
	if err != nil {
		return nil, err
	}
	idxType, err := c.checkExpr(e.Index)
	if err != nil {
		return nil, err
	}

	switch t := objType.(type) {
	case *ListType:
		if _, ok := idxType.(*IntType); !ok {
			return nil, fmt.Errorf("%d:%d: list index must be int, got %s", e.Pos.Line, e.Pos.Col, idxType)
		}
		return t.Elem, nil
	case *MapType:
		if !t.Key.Equals(idxType) {
			return nil, fmt.Errorf("%d:%d: map key type mismatch: expected %s, got %s", e.Pos.Line, e.Pos.Col, t.Key, idxType)
		}
		return t.Value, nil
	case *StrType:
		if _, ok := idxType.(*IntType); !ok {
			return nil, fmt.Errorf("%d:%d: string index must be int, got %s", e.Pos.Line, e.Pos.Col, idxType)
		}
		return &StrType{}, nil
	}

	return nil, fmt.Errorf("%d:%d: cannot index %s", e.Pos.Line, e.Pos.Col, objType)
}

func (c *Checker) checkAttrExpr(e *parser.AttrExpr) (Type, error) {
	// Special-case: super().<attr> — only resolves attr against the base class.
	if _, isSuper := e.Object.(*parser.SuperExpr); isSuper {
		if c.currentClass == nil {
			return nil, fmt.Errorf("%d:%d: 'super()' is only valid inside a class method", e.Pos.Line, e.Pos.Col)
		}
		if c.currentClass.Base == nil {
			return nil, fmt.Errorf("%d:%d: 'super()' used in class %s which has no base", e.Pos.Line, e.Pos.Col, c.currentClass.Name)
		}
		base := c.currentClass.Base
		// __init__ isn't listed in Methods (it's a constructor), resolve specially.
		if e.Attr == "__init__" {
			sig := findInitSig(base)
			if sig == nil {
				sig = &FuncType{Params: []Type{}, Return: &NoneType{}}
			}
			e.Object.SetResolvedType(&InstanceType{Class: base})
			return sig, nil
		}
		sig, ok := base.Methods[e.Attr]
		if !ok {
			return nil, fmt.Errorf("%d:%d: base class %s has no method %s", e.Pos.Line, e.Pos.Col, base.Name, e.Attr)
		}
		e.Object.SetResolvedType(&InstanceType{Class: base})
		return sig, nil
	}

	objType, err := c.checkExpr(e.Object)
	if err != nil {
		return nil, err
	}

	switch t := objType.(type) {
	case *ListType:
		if e.Attr == "append" {
			// Returns a callable that takes one element and returns None
			return &FuncType{Params: []Type{t.Elem}, Return: &NoneType{}}, nil
		}
	case *ModuleType:
		exp, ok := t.Exports[e.Attr]
		if !ok {
			return nil, fmt.Errorf("%d:%d: module %s has no export %s", e.Pos.Line, e.Pos.Col, t.ID, e.Attr)
		}
		return exp, nil
	case *InstanceType:
		if idx, ok := t.Class.FieldIdx[e.Attr]; ok {
			return t.Class.Fields[idx].Type, nil
		}
		if sig, ok := t.Class.Methods[e.Attr]; ok {
			return sig, nil
		}
		return nil, fmt.Errorf("%d:%d: %s has no attribute %s", e.Pos.Line, e.Pos.Col, t.Class.Name, e.Attr)
	}

	return nil, fmt.Errorf("%d:%d: %s has no attribute %s", e.Pos.Line, e.Pos.Col, objType, e.Attr)
}

// findInitSig returns the signature of __init__ on ct (walking base chain), or
// nil if none is defined.
func findInitSig(ct *ClassType) *FuncType {
	// __init__ is stored neither in Methods (it's not a normal method) nor
	// accessible via Methods. We record it the same way but flag it. For now,
	// the parser treats __init__ as just another method, so it's in Methods.
	if sig, ok := ct.Methods["__init__"]; ok {
		return sig
	}
	if ct.Base != nil {
		return findInitSig(ct.Base)
	}
	return nil
}

func (c *Checker) checkListLit(e *parser.ListLit) (Type, error) {
	if len(e.Elements) == 0 {
		return nil, fmt.Errorf("%d:%d: cannot infer type of empty list literal, use type annotation", e.Pos.Line, e.Pos.Col)
	}

	elemType, err := c.checkExpr(e.Elements[0])
	if err != nil {
		return nil, err
	}

	for i := 1; i < len(e.Elements); i++ {
		et, err := c.checkExpr(e.Elements[i])
		if err != nil {
			return nil, err
		}
		// Widen to a common type for instance types.
		if merged, ok := mergeInstanceTypes(elemType, et); ok {
			elemType = merged
			continue
		}
		if !elemType.Equals(et) {
			return nil, fmt.Errorf("%d:%d: list elements must all be %s, got %s", e.Pos.Line, e.Pos.Col, elemType, et)
		}
	}

	return &ListType{Elem: elemType}, nil
}

// mergeInstanceTypes returns the least-common-ancestor (as an InstanceType) for
// two instance types. If either input is not an InstanceType or no common
// ancestor exists, returns nil, false.
func mergeInstanceTypes(a, b Type) (Type, bool) {
	ai, okA := a.(*InstanceType)
	bi, okB := b.(*InstanceType)
	if !okA || !okB {
		return nil, false
	}
	ancestors := map[*ClassType]bool{}
	for c := ai.Class; c != nil; c = c.Base {
		ancestors[c] = true
	}
	for c := bi.Class; c != nil; c = c.Base {
		if ancestors[c] {
			return &InstanceType{Class: c}, true
		}
	}
	return nil, false
}

func (c *Checker) checkMapLit(e *parser.MapLit) (Type, error) {
	if len(e.Keys) == 0 {
		return nil, fmt.Errorf("%d:%d: cannot infer type of empty map literal, use type annotation", e.Pos.Line, e.Pos.Col)
	}

	keyType, err := c.checkExpr(e.Keys[0])
	if err != nil {
		return nil, err
	}
	valType, err := c.checkExpr(e.Values[0])
	if err != nil {
		return nil, err
	}

	for i := 1; i < len(e.Keys); i++ {
		kt, err := c.checkExpr(e.Keys[i])
		if err != nil {
			return nil, err
		}
		if !keyType.Equals(kt) {
			return nil, fmt.Errorf("%d:%d: map keys must all be %s, got %s", e.Pos.Line, e.Pos.Col, keyType, kt)
		}
		vt, err := c.checkExpr(e.Values[i])
		if err != nil {
			return nil, err
		}
		if !valType.Equals(vt) {
			return nil, fmt.Errorf("%d:%d: map values must all be %s, got %s", e.Pos.Line, e.Pos.Col, valType, vt)
		}
	}

	return &MapType{Key: keyType, Value: valType}, nil
}

// binaryDunderName maps a binary operator token to its corresponding dunder
// method name. Returns "" for operators without a dunder analogue.
func binaryDunderName(op string) string {
	switch op {
	case "+":
		return "__add__"
	case "-":
		return "__sub__"
	case "*":
		return "__mul__"
	case "/":
		return "__truediv__"
	case "//":
		return "__floordiv__"
	case "%":
		return "__mod__"
	case "**":
		return "__pow__"
	case "==":
		return "__eq__"
	case "!=":
		return "__ne__"
	case "<":
		return "__lt__"
	case "<=":
		return "__le__"
	case ">":
		return "__gt__"
	case ">=":
		return "__ge__"
	}
	return ""
}

func (c *Checker) resolveTypeAnnotation(ann *parser.TypeAnnotation) Type {
	switch ann.Name {
	case "int":
		return &IntType{}
	case "float":
		return &FloatType{}
	case "bool":
		return &BoolType{}
	case "str":
		return &StrType{}
	case "None":
		return &NoneType{}
	case "list":
		if len(ann.Params) != 1 {
			return nil
		}
		elem := c.resolveTypeAnnotation(ann.Params[0])
		if elem == nil {
			return nil
		}
		return &ListType{Elem: elem}
	case "map":
		if len(ann.Params) != 2 {
			return nil
		}
		key := c.resolveTypeAnnotation(ann.Params[0])
		val := c.resolveTypeAnnotation(ann.Params[1])
		if key == nil || val == nil {
			return nil
		}
		return &MapType{Key: key, Value: val}
	}
	// Fall back to class lookup.
	if ct, ok := c.Env.LookupClass(ann.Name); ok {
		return &InstanceType{Class: ct}
	}
	return nil
}
