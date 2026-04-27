package types

import (
	"fmt"

	"github.com/yehoyadashtinmetz/spython/parser"
)

type Checker struct {
	Env               *Env
	currentReturnType Type
	// currentYieldType is the element type T of the enclosing generator
	// function's declared `Iterator[T]` return. Non-nil only inside a
	// generator body; populated by checkFuncDef and consulted by
	// checkYieldStmt and checkReturnStmt (return-with-value is rejected
	// in generators).
	currentYieldType Type
	ModuleID          string // ID of the module being checked; set by loader
	imports           map[string]*ModuleType

	// Method-body context (populated only while checking a method's body).
	currentClass *ClassType // nil when not inside a class body
	// classIDSrc points at the next-id counter used by registerClassType.
	// Defaults to a per-checker int so direct NewChecker users still work,
	// but the loader overrides it via SetClassIDSource so every class across
	// every module in one Load gets a unique id — required for isinstance
	// (which compares class_id at runtime) to be correct across modules.
	classIDSrc *int

	// Exception-handler depth tracking. Non-zero inside an `except` clause
	// (so raise-class checks could later be relaxed for bare `raise`, not in
	// v1). `finallyDepth` is set while checking a `finally` body and bans
	// `return`, `break`, `continue` there — v1 scope restriction.
	finallyDepth int

	// typeHint is an expected-type bias set by statements that have a
	// concrete annotation (e.g. `x: list[str] = []`). Expression checkers
	// may consult it when literal syntax is ambiguous — today only the
	// empty list literal uses it. Cleared by each setter via defer/restore.
	typeHint Type
}

func NewChecker() *Checker {
	var ctr int
	c := &Checker{Env: NewEnv(), classIDSrc: &ctr}
	c.registerBuiltins()
	return c
}

func NewCheckerWithImports(moduleID string, imports map[string]*ModuleType) *Checker {
	var ctr int
	c := &Checker{
		Env:        NewEnv(),
		ModuleID:   moduleID,
		imports:    imports,
		classIDSrc: &ctr,
	}
	c.registerBuiltins()
	return c
}

// SetClassIDSource overrides the per-checker class ID counter with a
// caller-owned counter. The loader uses this to share a single sequence
// across every module in one compilation so isinstance class_id comparisons
// are unique program-wide.
func (c *Checker) SetClassIDSource(p *int) {
	c.classIDSrc = p
}

func (c *Checker) registerBuiltins() {
	// print is special — handled in checkCallExpr
	// len is special — handled in checkCallExpr
	// range is special — handled in checkCallExpr
}

// InjectBuiltins makes the given classes available in this checker's env as
// both values (so `Exception(msg)` / `isinstance(x, Exception)` work) and as
// class names (so type annotations and base-class references resolve). The
// loader uses this to share a single canonical Exception hierarchy across
// every module — pointer identity must hold for IsSubclassOf to keep working
// across module boundaries. Must be called before Check().
func (c *Checker) InjectBuiltins(classes map[string]*ClassType) {
	for name, ct := range classes {
		c.Env.DefineClass(name, ct)
		c.Env.Define(name, ct)
	}
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
		// Mirror imported classes into the class table so they resolve in
		// type annotations (`f: File = ...`) and as base classes.
		if ct, ok := t.(*ClassType); ok {
			c.Env.DefineClass(effective, ct)
		}
	}
	return nil
}

// Exports returns the public surface of this module: top-level functions,
// classes, and top-level constant assignments (annotated or inferred).
// Must be called after Check() succeeds.
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
		case *parser.ClassDef:
			if ct, ok := c.Env.LookupClass(s.Name); ok {
				out[s.Name] = ct
			}
		case *parser.AssignStmt:
			if t, ok := c.Env.Lookup(s.Name); ok {
				out[s.Name] = t
			}
		}
	}
	return out
}

func (c *Checker) registerFuncSignature(s *parser.FuncDef) error {
	paramTypes := []Type{}
	paramNames := []string{}
	paramDefaults := []parser.Expr{}
	kwOnlyStart := -1
	var varArgsElem Type
	varArgsName := ""
	var kwargsElem Type
	kwargsName := ""

	for _, p := range s.Params {
		if p.TypeAnn == nil {
			// `self` is allowed unannotated; for *args/**kwargs we still
			// enforce annotations.
			if p.Kind != parser.ParamPositional {
				return fmt.Errorf("%d:%d: %s parameter requires a type annotation",
					s.Pos.Line, s.Pos.Col, p.Name)
			}
			return fmt.Errorf("%d:%d: parameter %s requires a type annotation", s.Pos.Line, s.Pos.Col, p.Name)
		}
		pt := c.resolveTypeAnnotation(p.TypeAnn)
		if pt == nil {
			return fmt.Errorf("%d:%d: unknown parameter type: %s", s.Pos.Line, s.Pos.Col, p.TypeAnn.Name)
		}
		switch p.Kind {
		case parser.ParamVarArgs:
			varArgsElem = pt
			varArgsName = p.Name
			if kwOnlyStart == -1 {
				kwOnlyStart = len(paramTypes)
			}
		case parser.ParamKwargs:
			kwargsElem = pt
			kwargsName = p.Name
		default:
			if p.Default != nil {
				prevHint := c.typeHint
				c.typeHint = pt
				defaultType, err := c.checkExpr(p.Default)
				c.typeHint = prevHint
				if err != nil {
					return fmt.Errorf("%d:%d: parameter %s: default value: %w",
						s.Pos.Line, s.Pos.Col, p.Name, err)
				}
				if !IsAssignable(defaultType, pt) {
					return fmt.Errorf("%d:%d: parameter %s: default value of type %s is not assignable to %s",
						s.Pos.Line, s.Pos.Col, p.Name, defaultType, pt)
				}
			}
			paramTypes = append(paramTypes, pt)
			paramNames = append(paramNames, p.Name)
			paramDefaults = append(paramDefaults, p.Default)
		}
	}
	if kwOnlyStart == -1 {
		kwOnlyStart = len(paramTypes)
	}

	retType := Type(&NoneType{})
	if s.ReturnType != nil {
		retType = c.resolveTypeAnnotation(s.ReturnType)
		if retType == nil {
			return fmt.Errorf("%d:%d: unknown return type: %s", s.Pos.Line, s.Pos.Col, s.ReturnType.Name)
		}
	}

	if s.IsGenerator {
		if _, ok := retType.(*IteratorType); !ok {
			return fmt.Errorf("%d:%d: generator function %s must declare return type Iterator[T]",
				s.Pos.Line, s.Pos.Col, s.Name)
		}
		if s.Extern {
			return fmt.Errorf("%d:%d: @extern function %s cannot be a generator",
				s.Pos.Line, s.Pos.Col, s.Name)
		}
	}

	if s.Extern {
		if varArgsElem != nil || kwargsElem != nil {
			return fmt.Errorf("%d:%d: @extern %s: *args and **kwargs are not supported on @extern functions",
				s.Pos.Line, s.Pos.Col, s.Name)
		}
		for _, p := range s.Params {
			if p.Default != nil {
				return fmt.Errorf("%d:%d: @extern %s: parameter %s cannot have a default value",
					s.Pos.Line, s.Pos.Col, s.Name, p.Name)
			}
		}
		for i, pt := range paramTypes {
			if !isFFIMarshallable(pt) {
				return fmt.Errorf("%d:%d: @extern %s: parameter %s has non-marshallable type %s",
					s.Pos.Line, s.Pos.Col, s.Name, s.Params[i].Name, pt)
			}
		}
		if !isFFIMarshallableReturn(retType) {
			return fmt.Errorf("%d:%d: @extern %s: return type %s is not marshallable",
				s.Pos.Line, s.Pos.Col, s.Name, retType)
		}
	}

	funcType := &FuncType{
		Params:        paramTypes,
		ParamNames:    paramNames,
		ParamDefaults: paramDefaults,
		KwOnlyStart:   kwOnlyStart,
		VarArgsElem:   varArgsElem,
		VarArgsName:   varArgsName,
		KwargsElem:    kwargsElem,
		KwargsName:    kwargsName,
		Return:        retType,
		DefinedIn:     c.ModuleID,
		ExternSymbol:  s.ExternSymbol,
	}
	c.Env.Define(s.Name, funcType)
	return nil
}

// isFFIMarshallable reports whether t is an allowed parameter type for an
// @extern function in v1. Current set: int, float, bool, str, bytes, bytearray.
// Lists, maps, tuples, class instances, and None are not yet supported as
// parameters.
func isFFIMarshallable(t Type) bool {
	switch t.(type) {
	case *IntType, *FloatType, *BoolType, *StrType, *BytesType, *BytearrayType:
		return true
	}
	return false
}

// isFFIMarshallableReturn extends the marshallable set with None for return
// position (C void).
func isFFIMarshallableReturn(t Type) bool {
	if _, ok := t.(*NoneType); ok {
		return true
	}
	return isFFIMarshallable(t)
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
	*c.classIDSrc++
	ct := &ClassType{
		Name:       s.Name,
		Base:       base,
		FieldIdx:   map[string]int{},
		Methods:    map[string]*FuncType{},
		OwnMethods: map[string]bool{},
		MethodSrc:  map[string]*ClassType{},
		ClassID:    *c.classIDSrc,
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
		paramNames := []string{}
		paramDefaults := []parser.Expr{}
		kwOnlyStart := -1
		var varArgsElem Type
		varArgsName := ""
		var kwargsElem Type
		kwargsName := ""
		if m.Params[0].Default != nil {
			return fmt.Errorf("%d:%d: method %s.%s: 'self' cannot have a default value",
				m.Pos.Line, m.Pos.Col, s.Name, m.Name)
		}
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
			switch p.Kind {
			case parser.ParamVarArgs:
				varArgsElem = pt
				varArgsName = p.Name
				if kwOnlyStart == -1 {
					kwOnlyStart = len(paramTypes)
				}
			case parser.ParamKwargs:
				kwargsElem = pt
				kwargsName = p.Name
			default:
				if p.Default != nil {
					prevHint := c.typeHint
					c.typeHint = pt
					defaultType, err := c.checkExpr(p.Default)
					c.typeHint = prevHint
					if err != nil {
						return fmt.Errorf("%d:%d: method %s.%s: parameter %s: default value: %w",
							m.Pos.Line, m.Pos.Col, s.Name, m.Name, p.Name, err)
					}
					if !IsAssignable(defaultType, pt) {
						return fmt.Errorf("%d:%d: method %s.%s: parameter %s: default value of type %s is not assignable to %s",
							m.Pos.Line, m.Pos.Col, s.Name, m.Name, p.Name, defaultType, pt)
					}
				}
				paramTypes = append(paramTypes, pt)
				paramNames = append(paramNames, p.Name)
				paramDefaults = append(paramDefaults, p.Default)
			}
		}
		if kwOnlyStart == -1 {
			kwOnlyStart = len(paramTypes)
		}
		retType := Type(&NoneType{})
		if m.ReturnType != nil {
			retType = c.resolveTypeAnnotation(m.ReturnType)
			if retType == nil {
				return fmt.Errorf("%d:%d: method %s.%s: unknown return type %s",
					m.Pos.Line, m.Pos.Col, s.Name, m.Name, m.ReturnType.Name)
			}
		}
		sig := &FuncType{
			Params:        paramTypes,
			ParamNames:    paramNames,
			ParamDefaults: paramDefaults,
			KwOnlyStart:   kwOnlyStart,
			VarArgsElem:   varArgsElem,
			VarArgsName:   varArgsName,
			KwargsElem:    kwargsElem,
			KwargsName:    kwargsName,
			Return:        retType,
			DefinedIn:     c.ModuleID,
		}

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
		case *parser.TryStmt:
			if err := c.inferFieldsFromStmts(ct, s.Body); err != nil {
				return err
			}
			for _, ec := range s.Excepts {
				if err := c.inferFieldsFromStmts(ct, ec.Body); err != nil {
					return err
				}
			}
			if s.HasFinally {
				if err := c.inferFieldsFromStmts(ct, s.FinallyBody); err != nil {
					return err
				}
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
	case *parser.MultiAssignStmt:
		return c.checkMultiAssignStmt(s)
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
		if c.finallyDepth > 0 {
			return fmt.Errorf("%d:%d: `return` inside a `finally` block is not supported in v1",
				s.Pos.Line, s.Pos.Col)
		}
		return c.checkReturnStmt(s)
	case *parser.BreakStmt:
		if c.finallyDepth > 0 {
			return fmt.Errorf("%d:%d: `break` inside a `finally` block is not supported in v1",
				s.Pos.Line, s.Pos.Col)
		}
		return nil
	case *parser.ContinueStmt:
		if c.finallyDepth > 0 {
			return fmt.Errorf("%d:%d: `continue` inside a `finally` block is not supported in v1",
				s.Pos.Line, s.Pos.Col)
		}
		return nil
	case *parser.TryStmt:
		return c.checkTryStmt(s)
	case *parser.RaiseStmt:
		return c.checkRaiseStmt(s)
	case *parser.YieldStmt:
		return c.checkYieldStmt(s)
	default:
		return fmt.Errorf("unknown statement type: %T", stmt)
	}
}

func (c *Checker) checkYieldStmt(s *parser.YieldStmt) error {
	if c.currentYieldType == nil {
		return fmt.Errorf("%d:%d: 'yield' outside generator function", s.Pos.Line, s.Pos.Col)
	}
	if c.finallyDepth > 0 {
		return fmt.Errorf("%d:%d: 'yield' inside a 'finally' block is not supported in v1",
			s.Pos.Line, s.Pos.Col)
	}
	valType, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}
	if s.IsFrom {
		it, ok := valType.(*IteratorType)
		if !ok {
			return fmt.Errorf("%d:%d: 'yield from' requires Iterator[T], got %s", s.Pos.Line, s.Pos.Col, valType)
		}
		if !IsAssignable(it.Elem, c.currentYieldType) {
			return fmt.Errorf("%d:%d: 'yield from' element type mismatch: expected %s, got %s",
				s.Pos.Line, s.Pos.Col, c.currentYieldType, it.Elem)
		}
		return nil
	}
	if !IsAssignable(valType, c.currentYieldType) {
		return fmt.Errorf("%d:%d: yield type mismatch: expected %s, got %s",
			s.Pos.Line, s.Pos.Col, c.currentYieldType, valType)
	}
	return nil
}

func (c *Checker) checkTryStmt(s *parser.TryStmt) error {
	// Try body uses the current scope; no push/pop needed.
	for _, stmt := range s.Body {
		if err := c.checkStmt(stmt); err != nil {
			return err
		}
	}
	excBase, _ := c.Env.LookupClass("Exception")
	for i, ec := range s.Excepts {
		var excClass *ClassType
		if ec.ExcType != nil {
			t := c.resolveTypeAnnotation(ec.ExcType)
			if inst, ok := t.(*InstanceType); ok {
				excClass = inst.Class
			} else {
				return fmt.Errorf("%d:%d: except target must name a class, got %s",
					ec.Pos.Line, ec.Pos.Col, ec.ExcType.Name)
			}
			if excBase != nil && !excClass.IsSubclassOf(excBase) {
				return fmt.Errorf("%d:%d: catching classes that do not inherit from Exception is not allowed (%s)",
					ec.Pos.Line, ec.Pos.Col, excClass.Name)
			}
		} else if i != len(s.Excepts)-1 {
			return fmt.Errorf("%d:%d: bare `except:` must be the last except clause",
				ec.Pos.Line, ec.Pos.Col)
		}
		c.Env.Push()
		if ec.VarName != "" {
			if excClass == nil {
				return fmt.Errorf("%d:%d: bare `except:` cannot bind a variable — use `except T as %s:` instead",
					ec.Pos.Line, ec.Pos.Col, ec.VarName)
			}
			c.Env.Define(ec.VarName, &InstanceType{Class: excClass})
		}
		for _, stmt := range ec.Body {
			if err := c.checkStmt(stmt); err != nil {
				c.Env.Pop()
				return err
			}
		}
		c.Env.Pop()
	}
	if s.HasFinally {
		c.finallyDepth++
		for _, stmt := range s.FinallyBody {
			if err := c.checkStmt(stmt); err != nil {
				c.finallyDepth--
				return err
			}
		}
		c.finallyDepth--
	}
	return nil
}

func (c *Checker) checkRaiseStmt(s *parser.RaiseStmt) error {
	t, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}
	inst, ok := t.(*InstanceType)
	if !ok {
		return fmt.Errorf("%d:%d: exceptions must derive from Exception, cannot raise %s",
			s.Pos.Line, s.Pos.Col, t)
	}
	if excBase, ok := c.Env.LookupClass("Exception"); ok {
		if !inst.Class.IsSubclassOf(excBase) {
			return fmt.Errorf("%d:%d: %s does not inherit from Exception",
				s.Pos.Line, s.Pos.Col, inst.Class.Name)
		}
	}
	return nil
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
	posIdx := 0
	for i := 1; i < len(m.Params); i++ {
		p := m.Params[i]
		switch p.Kind {
		case parser.ParamVarArgs:
			c.Env.Define(p.Name, &ListType{Elem: sig.VarArgsElem})
		case parser.ParamKwargs:
			c.Env.Define(p.Name, &MapType{Key: &StrType{}, Value: sig.KwargsElem})
		default:
			c.Env.Define(p.Name, sig.Params[posIdx])
			posIdx++
		}
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
	// If the LHS carries an annotation, bias the RHS check toward that type
	// so ambiguous literals (empty list) can resolve.
	var annType Type
	if s.TypeAnn != nil {
		annType = c.resolveTypeAnnotation(s.TypeAnn)
		prev := c.typeHint
		c.typeHint = annType
		defer func() { c.typeHint = prev }()
	}

	valType, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}

	if s.TypeAnn != nil {
		if annType == nil {
			return fmt.Errorf("%d:%d: unknown type: %s", s.TypeAnn.Pos.Line, s.TypeAnn.Pos.Col, s.TypeAnn.Name)
		}
		if !IsAssignable(valType, annType) {
			return fmt.Errorf("%d:%d: cannot assign %s to %s", s.Pos.Line, s.Pos.Col, valType, annType)
		}
		c.Env.Define(s.Name, annType)
	} else if existing, ok := c.Env.Lookup(s.Name); ok {
		// Reassignment: verify compatibility with existing type.
		if !IsAssignable(valType, existing) {
			return fmt.Errorf("%d:%d: cannot assign %s to %s", s.Pos.Line, s.Pos.Col, valType, existing)
		}
	} else {
		// First binding without annotation: infer type from the RHS.
		if _, isNone := valType.(*NoneType); isNone {
			return fmt.Errorf("%d:%d: cannot infer type of %s from None; add a type annotation",
				s.Pos.Line, s.Pos.Col, s.Name)
		}
		c.Env.Define(s.Name, valType)
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

func (c *Checker) checkMultiAssignStmt(s *parser.MultiAssignStmt) error {
	vt, err := c.checkExpr(s.Value)
	if err != nil {
		return err
	}
	tt, ok := vt.(*TupleType)
	if !ok {
		return fmt.Errorf("%d:%d: multi-assign RHS must be a tuple, got %s", s.Pos.Line, s.Pos.Col, vt)
	}
	if len(tt.Elements) != len(s.Names) {
		return fmt.Errorf("%d:%d: tuple arity mismatch: %d names vs %d elements",
			s.Pos.Line, s.Pos.Col, len(s.Names), len(tt.Elements))
	}
	for i, name := range s.Names {
		// If name exists already, require compatible type; otherwise bind it.
		if existing, ok := c.Env.Lookup(name); ok {
			if !IsAssignable(tt.Elements[i], existing) {
				return fmt.Errorf("%d:%d: cannot assign %s to %s (declared %s)",
					s.Pos.Line, s.Pos.Col, tt.Elements[i], name, existing)
			}
		} else {
			c.Env.Define(name, tt.Elements[i])
		}
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
	case *BytearrayType:
		if _, ok := idxType.(*IntType); !ok {
			return fmt.Errorf("%d:%d: bytearray index must be int, got %s", s.Pos.Line, s.Pos.Col, idxType)
		}
		if _, ok := valType.(*IntType); !ok {
			return fmt.Errorf("%d:%d: bytearray element must be int (0-255), got %s", s.Pos.Line, s.Pos.Col, valType)
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
	case *IteratorType:
		c.Env.Define(s.VarName, t.Elem)
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
	// Extern functions have no body — their signature was already validated in
	// registerFuncSignature, which also enforces the marshallable type set.
	if s.Extern {
		return nil
	}

	// Reuse the FuncType already registered by registerFuncSignature so that
	// *args/**kwargs metadata stays consistent.
	existingSym, ok := c.Env.Lookup(s.Name)
	var funcType *FuncType
	if ok {
		funcType, _ = existingSym.(*FuncType)
	}
	if funcType == nil {
		// Fall back to recomputing (defensive): build from params, no varargs.
		paramTypes := []Type{}
		paramNames := []string{}
		for _, p := range s.Params {
			if p.Kind != parser.ParamPositional {
				return fmt.Errorf("%d:%d: internal: signature for %s not pre-registered", s.Pos.Line, s.Pos.Col, s.Name)
			}
			if p.TypeAnn == nil {
				return fmt.Errorf("%d:%d: parameter %s requires a type annotation", s.Pos.Line, s.Pos.Col, p.Name)
			}
			pt := c.resolveTypeAnnotation(p.TypeAnn)
			if pt == nil {
				return fmt.Errorf("%d:%d: unknown parameter type: %s", s.Pos.Line, s.Pos.Col, p.TypeAnn.Name)
			}
			paramTypes = append(paramTypes, pt)
			paramNames = append(paramNames, p.Name)
		}
		retType := Type(&NoneType{})
		if s.ReturnType != nil {
			retType = c.resolveTypeAnnotation(s.ReturnType)
			if retType == nil {
				return fmt.Errorf("%d:%d: unknown return type: %s", s.Pos.Line, s.Pos.Col, s.ReturnType.Name)
			}
		}
		funcType = &FuncType{
			Params:      paramTypes,
			ParamNames:  paramNames,
			KwOnlyStart: len(paramTypes),
			Return:      retType,
			DefinedIn:   c.ModuleID,
		}
		c.Env.Define(s.Name, funcType)
	}
	retType := funcType.Return

	// Check body in new scope
	prevReturnType := c.currentReturnType
	prevYieldType := c.currentYieldType
	c.currentReturnType = retType
	if s.IsGenerator {
		// retType was validated as IteratorType in registerFuncSignature.
		c.currentYieldType = retType.(*IteratorType).Elem
	} else {
		c.currentYieldType = nil
	}
	c.Env.Push()
	for _, p := range s.Params {
		switch p.Kind {
		case parser.ParamVarArgs:
			c.Env.Define(p.Name, &ListType{Elem: funcType.VarArgsElem})
		case parser.ParamKwargs:
			c.Env.Define(p.Name, &MapType{Key: &StrType{}, Value: funcType.KwargsElem})
		default:
			pt := c.resolveTypeAnnotation(p.TypeAnn)
			c.Env.Define(p.Name, pt)
		}
	}
	for _, stmt := range s.Body {
		if err := c.checkStmt(stmt); err != nil {
			return err
		}
	}
	c.Env.Pop()
	c.currentReturnType = prevReturnType
	c.currentYieldType = prevYieldType

	return nil
}

func (c *Checker) checkReturnStmt(s *parser.ReturnStmt) error {
	// Inside a generator, only bare `return` (equivalent to `raise
	// StopIteration`) is allowed. `return value` is a v1 restriction
	// because we don't yet model the StopIteration value channel.
	if c.currentYieldType != nil {
		if s.Value != nil {
			return fmt.Errorf("%d:%d: 'return' with a value is not allowed inside a generator", s.Pos.Line, s.Pos.Col)
		}
		return nil
	}
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
	case *parser.BytesLit:
		t = &BytesType{}
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
	case *parser.TupleLit:
		t, err = c.checkTupleLit(e)
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
		// Builtins (and constructor calls below the switch) don't accept
		// keyword args or unpacks. Reject them up front for a clearer error.
		if e.HasVariadicForms() && isBuiltinName(ident.Name) {
			return nil, fmt.Errorf("%d:%d: %s() does not accept keyword arguments or *args/**kwargs unpacking",
				e.Pos.Line, e.Pos.Col, ident.Name)
		}
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
			case *StrType, *BytesType, *BytearrayType, *ListType, *MapType:
				return &IntType{}, nil
			}
			return nil, fmt.Errorf("%d:%d: len() not supported for %s", e.Pos.Line, e.Pos.Col, argType)

		case "next":
			if len(e.Args) != 1 {
				return nil, fmt.Errorf("%d:%d: next() takes exactly 1 argument", e.Pos.Line, e.Pos.Col)
			}
			argType, err := c.checkExpr(e.Args[0])
			if err != nil {
				return nil, err
			}
			it, ok := argType.(*IteratorType)
			if !ok {
				return nil, fmt.Errorf("%d:%d: next() requires Iterator[T], got %s", e.Pos.Line, e.Pos.Col, argType)
			}
			return it.Elem, nil

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

		case "bytes":
			if len(e.Args) != 1 {
				return nil, fmt.Errorf("%d:%d: bytes() takes exactly 1 argument", e.Pos.Line, e.Pos.Col)
			}
			argType, err := c.checkExpr(e.Args[0])
			if err != nil {
				return nil, err
			}
			switch argType.(type) {
			case *StrType, *BytesType, *BytearrayType:
				return &BytesType{}, nil
			}
			return nil, fmt.Errorf("%d:%d: cannot convert %s to bytes", e.Pos.Line, e.Pos.Col, argType)

		case "bytearray":
			if len(e.Args) != 1 {
				return nil, fmt.Errorf("%d:%d: bytearray() takes exactly 1 argument", e.Pos.Line, e.Pos.Col)
			}
			argType, err := c.checkExpr(e.Args[0])
			if err != nil {
				return nil, err
			}
			switch argType.(type) {
			case *IntType, *BytesType, *BytearrayType:
				return &BytearrayType{}, nil
			}
			return nil, fmt.Errorf("%d:%d: cannot convert %s to bytearray", e.Pos.Line, e.Pos.Col, argType)
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
		// Synthesize a no-arg signature when __init__ is absent.
		if initSig == nil {
			initSig = &FuncType{}
		}
		if err := c.matchCallArgs(initSig, e, fmt.Sprintf("%s constructor", ct.Name)); err != nil {
			return nil, err
		}
		return &InstanceType{Class: ct}, nil
	}

	funcType, ok := funcExpr.(*FuncType)
	if !ok {
		return nil, fmt.Errorf("%d:%d: %s is not callable", e.Pos.Line, e.Pos.Col, funcExpr)
	}

	if err := c.matchCallArgs(funcType, e, "function"); err != nil {
		return nil, err
	}

	return funcType.Return, nil
}

// paramName returns the name of param i, or a synthetic placeholder when
// ParamNames isn't populated (legacy FuncType constructions).
func paramName(ft *FuncType, i int) string {
	if i < len(ft.ParamNames) {
		return ft.ParamNames[i]
	}
	return fmt.Sprintf("#%d", i+1)
}

// matchCallArgs validates a CallExpr against a FuncType, supporting positional
// args, *list unpack, name=value kwargs, and **dict unpack.
func (c *Checker) matchCallArgs(ft *FuncType, e *parser.CallExpr, who string) error {
	pos := e.Pos
	nNamed := len(ft.Params) // total named (positional + kw-only) slots
	kwOnly := ft.KwOnlyStart
	// When the signature lacks *args, there can be no kw-only params; this
	// also normalizes synthetic FuncTypes (e.g. list.append) that don't set
	// KwOnlyStart explicitly.
	if ft.VarArgsElem == nil {
		kwOnly = nNamed
	}
	if kwOnly < 0 || kwOnly > nNamed {
		kwOnly = nNamed
	}

	filled := make([]bool, nNamed)
	hasStarUnpack := false

	// Process positional args (and *expr unpacks).
	posIdx := 0
	for i, arg := range e.Args {
		argType, err := c.checkExpr(arg)
		if err != nil {
			return err
		}
		if e.IsArgStar(i) {
			lt, ok := argType.(*ListType)
			if !ok {
				return fmt.Errorf("%d:%d: %s: *unpack argument must be a list, got %s",
					pos.Line, pos.Col, who, argType)
			}
			if ft.VarArgsElem == nil {
				return fmt.Errorf("%d:%d: %s does not accept *args; cannot use *unpack",
					pos.Line, pos.Col, who)
			}
			// Mark all named positional slots as "must already be filled" — we
			// can't statically count what *unpack will produce.
			if posIdx < kwOnly {
				return fmt.Errorf("%d:%d: %s: *unpack appears before all named positional parameters are filled",
					pos.Line, pos.Col, who)
			}
			if !IsAssignable(lt.Elem, ft.VarArgsElem) {
				return fmt.Errorf("%d:%d: %s: *unpack element type %s not assignable to *args type %s",
					pos.Line, pos.Col, who, lt.Elem, ft.VarArgsElem)
			}
			hasStarUnpack = true
			continue
		}
		// Plain positional.
		if posIdx < kwOnly {
			if !IsAssignable(argType, ft.Params[posIdx]) {
				return fmt.Errorf("%d:%d: %s argument %d: expected %s, got %s",
					pos.Line, pos.Col, who, posIdx+1, ft.Params[posIdx], argType)
			}
			filled[posIdx] = true
			posIdx++
		} else if ft.VarArgsElem != nil {
			if !IsAssignable(argType, ft.VarArgsElem) {
				return fmt.Errorf("%d:%d: %s: *args element expected %s, got %s",
					pos.Line, pos.Col, who, ft.VarArgsElem, argType)
			}
			posIdx++
		} else {
			// Either we've spilled past the named positionals (overflow) or
			// we're hitting a kw-only slot positionally.
			if posIdx < nNamed {
				return fmt.Errorf("%d:%d: %s: parameter %s is keyword-only and cannot be passed positionally",
					pos.Line, pos.Col, who, paramName(ft, posIdx))
			}
			return fmt.Errorf("%d:%d: %s: too many positional arguments (expected %d)",
				pos.Line, pos.Col, who, kwOnly)
		}
	}

	// Process keyword args and **expr unpacks.
	hasDStar := false
	for _, kw := range e.Kwargs {
		valType, err := c.checkExpr(kw.Value)
		if err != nil {
			return err
		}
		if kw.IsDStar {
			mt, ok := valType.(*MapType)
			if !ok {
				return fmt.Errorf("%d:%d: %s: **unpack argument must be a map, got %s",
					pos.Line, pos.Col, who, valType)
			}
			if _, isStr := mt.Key.(*StrType); !isStr {
				return fmt.Errorf("%d:%d: %s: **unpack map must have str keys, got %s",
					pos.Line, pos.Col, who, mt.Key)
			}
			if ft.KwargsElem == nil {
				return fmt.Errorf("%d:%d: %s does not accept **kwargs; cannot use **unpack",
					pos.Line, pos.Col, who)
			}
			if !IsAssignable(mt.Value, ft.KwargsElem) {
				return fmt.Errorf("%d:%d: %s: **unpack value type %s not assignable to **kwargs type %s",
					pos.Line, pos.Col, who, mt.Value, ft.KwargsElem)
			}
			hasDStar = true
			continue
		}
		// Named kwarg: try to match a named param first.
		matched := false
		for i := 0; i < nNamed; i++ {
			if i < len(ft.ParamNames) && ft.ParamNames[i] == kw.Name {
				if filled[i] {
					return fmt.Errorf("%d:%d: %s: parameter %s already supplied",
						pos.Line, pos.Col, who, kw.Name)
				}
				if !IsAssignable(valType, ft.Params[i]) {
					return fmt.Errorf("%d:%d: %s: parameter %s expected %s, got %s",
						pos.Line, pos.Col, who, kw.Name, ft.Params[i], valType)
				}
				filled[i] = true
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if ft.KwargsElem == nil {
			return fmt.Errorf("%d:%d: %s: unexpected keyword argument %q",
				pos.Line, pos.Col, who, kw.Name)
		}
		if !IsAssignable(valType, ft.KwargsElem) {
			return fmt.Errorf("%d:%d: %s: keyword argument %q expected %s, got %s",
				pos.Line, pos.Col, who, kw.Name, ft.KwargsElem, valType)
		}
	}

	// Every named param must be filled statically (by positional or named
	// kwarg) unless it has a default. **unpack only contributes to **kwargs
	// in v1 — it does not bind to named params, which keeps codegen
	// straightforward.
	for i := 0; i < nNamed; i++ {
		if filled[i] {
			continue
		}
		if i < len(ft.ParamDefaults) && ft.ParamDefaults[i] != nil {
			continue
		}
		return fmt.Errorf("%d:%d: %s: missing argument for parameter %s",
			pos.Line, pos.Col, who, paramName(ft, i))
	}
	_ = hasStarUnpack
	_ = hasDStar
	return nil
}

// isBuiltinName returns true for names we treat as built-in functions in
// checkCallExpr. Builtins receive only positional args.
func isBuiltinName(name string) bool {
	switch name {
	case "isinstance", "print", "len", "range", "next",
		"int", "float", "str", "bytes", "bytearray":
		return true
	}
	return false
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
	case *BytesType:
		if _, ok := idxType.(*IntType); !ok {
			return nil, fmt.Errorf("%d:%d: bytes index must be int, got %s", e.Pos.Line, e.Pos.Col, idxType)
		}
		// Unlike str[i] (returns a 1-char str), bytes[i] returns an int 0-255.
		return &IntType{}, nil
	case *BytearrayType:
		if _, ok := idxType.(*IntType); !ok {
			return nil, fmt.Errorf("%d:%d: bytearray index must be int, got %s", e.Pos.Line, e.Pos.Col, idxType)
		}
		return &IntType{}, nil
	case *TupleType:
		// Tuples hold heterogeneous types — the element type depends on the
		// concrete index, which therefore must be a compile-time integer
		// literal so the checker can pick the right slot.
		lit, ok := e.Index.(*parser.IntLit)
		if !ok {
			return nil, fmt.Errorf("%d:%d: tuple index must be an integer literal", e.Pos.Line, e.Pos.Col)
		}
		if lit.Value < 0 || int(lit.Value) >= len(t.Elements) {
			return nil, fmt.Errorf("%d:%d: tuple index %d out of range [0, %d)", e.Pos.Line, e.Pos.Col, lit.Value, len(t.Elements))
		}
		return t.Elements[lit.Value], nil
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
	case *BytearrayType:
		_ = t
		if e.Attr == "append" {
			return &FuncType{Params: []Type{&IntType{}}, Return: &NoneType{}}, nil
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
		// Accept when an enclosing annotation tells us the element type.
		if lt, ok := c.typeHint.(*ListType); ok {
			return lt, nil
		}
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

func (c *Checker) checkTupleLit(e *parser.TupleLit) (Type, error) {
	elems := make([]Type, len(e.Elements))
	for i, el := range e.Elements {
		t, err := c.checkExpr(el)
		if err != nil {
			return nil, err
		}
		elems[i] = t
	}
	return &TupleType{Elements: elems}, nil
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
	case "bytes":
		return &BytesType{}
	case "bytearray":
		return &BytearrayType{}
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
	case "Iterator":
		if len(ann.Params) != 1 {
			return nil
		}
		elem := c.resolveTypeAnnotation(ann.Params[0])
		if elem == nil {
			return nil
		}
		return &IteratorType{Elem: elem}
	case "tuple":
		if len(ann.Params) == 0 {
			return &TupleType{Elements: nil}
		}
		elems := make([]Type, len(ann.Params))
		for i, p := range ann.Params {
			et := c.resolveTypeAnnotation(p)
			if et == nil {
				return nil
			}
			elems[i] = et
		}
		return &TupleType{Elements: elems}
	}
	// Fall back to class lookup.
	if ct, ok := c.Env.LookupClass(ann.Name); ok {
		return &InstanceType{Class: ct}
	}
	return nil
}
