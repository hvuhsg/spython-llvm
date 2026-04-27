package codegen

import (
	"fmt"
	"math"
	"strings"

	"github.com/yehoyadashtinmetz/spython/parser"
	"github.com/yehoyadashtinmetz/spython/types"
)

// ModuleInput is the codegen-facing view of a module produced by the loader.
// Using a local interface keeps codegen decoupled from the loader package.
type ModuleInput struct {
	ID      string
	Program *parser.Program
	Deps    []string
	IsEntry bool
	// Classes lists the class types registered by the checker for this module,
	// in source order.
	Classes []*types.ClassType
}

type Generator struct {
	buf          strings.Builder
	tmpCounter   int
	lblCounter   int
	vars         map[string]varInfo
	moduleConsts map[string]varInfo // module-level names: own globals + from-imports
	scopeStack   []map[string]varInfo
	stringConsts []string
	funcs        []*parser.FuncDef
	inFunction   bool
	currentMod   string

	// Break/continue label stack
	breakLabels    []string
	continueLabels []string

	// Finally-target stack. Each entry describes a try-with-finally that is
	// currently open; break/continue/return inside its try body (or except
	// bodies) must route through finally.entryLabel instead of branching
	// directly to the loop/function exit.
	finallyStack []finallyFrame

	// Class codegen state
	classes        []*types.ClassType                  // all classes across all modules in declaration order
	classByName    map[string]*types.ClassType         // name -> class
	classModule    map[*types.ClassType]string         // class -> module ID it was defined in
	classDef       map[*types.ClassType]*parser.ClassDef
	methodSlots    map[*types.ClassType]map[string]int // class -> method name -> slot index
	slotOrder      map[*types.ClassType][]string       // class -> slot index -> method name (ordered)
	slotOwner      map[*types.ClassType][]*types.ClassType // class -> slot index -> owning class
	currentClass   *types.ClassType                    // class whose method is being emitted (for super())

	// Declared return type of the function currently being emitted. Used to
	// upcast return values when a subclass instance is returned through a
	// superclass-typed signature.
	currentReturnType    types.Type
	currentReturnLLVMType string

	// Tracks C symbol names already emitted as `declare` statements for
	// @extern functions. Prevents LLVM "invalid redefinition" errors when
	// multiple modules bind to the same C symbol via explicit @extern("name").
	declaredExterns map[string]bool
}

type varInfo struct {
	llvmName string
	typ      types.Type
}

func New() *Generator {
	return &Generator{
		vars:            make(map[string]varInfo),
		classByName:     map[string]*types.ClassType{},
		classModule:     map[*types.ClassType]string{},
		classDef:        map[*types.ClassType]*parser.ClassDef{},
		methodSlots:     map[*types.ClassType]map[string]int{},
		slotOrder:       map[*types.ClassType][]string{},
		slotOwner:       map[*types.ClassType][]*types.ClassType{},
		declaredExterns: map[string]bool{},
	}
}

// Generate compiles a single Program as a self-contained entry module.
// Kept for backwards compatibility with existing tests; new callers should
// use GenerateAll.
func (g *Generator) Generate(program *parser.Program) (string, error) {
	if program == nil {
		return "", nil
	}
	mod := &ModuleInput{ID: "main", Program: program, IsEntry: true}
	return g.GenerateAll([]*ModuleInput{mod}, mod)
}

// GenerateAll compiles multiple modules into a single LLVM IR string.
// modules must be in topological order (deps before dependents). entry
// must be the module whose top-level code runs in @main.
func (g *Generator) GenerateAll(modules []*ModuleInput, entry *ModuleInput) (string, error) {
	// Emit module header
	g.emitLine("; ModuleID = 'spython'")
	g.emitLine("source_filename = \"spython\"")
	g.emitLine("target triple = \"arm64-apple-macosx14.0.0\"")
	g.emitLine("")

	// Register all classes from all modules so we know the global set. Doing
	// this before string collection lets us add strings needed by synthesized
	// methods (e.g., auto-default __str__/__repr__).
	for _, m := range modules {
		for _, ct := range m.Classes {
			if existing, ok := g.classByName[ct.Name]; ok {
				return "", fmt.Errorf("duplicate class name %q: defined in modules %q and %q",
					ct.Name, g.classModule[existing], m.ID)
			}
			g.classes = append(g.classes, ct)
			g.classByName[ct.Name] = ct
			g.classModule[ct] = m.ID
		}
		for _, stmt := range m.Program.Stmts {
			if cd, ok := stmt.(*parser.ClassDef); ok {
				if ct, ok := g.classByName[cd.Name]; ok {
					g.classDef[ct] = cd
				}
			}
		}
	}

	// Collect string constants across all modules
	g.addStringConst(" ")
	// Built-in messages used by compiler-inserted runtime checks.
	g.addStringConst("integer division by zero")
	g.addStringConst("integer modulo by zero")
	g.addStringConst("float division by zero")
	g.addStringConst("float floor division by zero")
	g.addStringConst("float modulo")
	for _, m := range modules {
		for _, stmt := range m.Program.Stmts {
			g.collectStringsInStmt(stmt)
		}
	}
	// Also register strings needed by synthesized __str__/__repr__ for every
	// class (whether they'll be synthesized or not — cheap over-collection).
	for _, ct := range g.classes {
		g.addStringConst(ct.Name + "(")
		g.addStringConst(")")
		g.addStringConst(", ")
		for _, f := range ct.Fields {
			g.addStringConst(f.Name + "=")
		}
	}

	// Emit string constants
	for i, s := range g.stringConsts {
		escaped := g.escapeString(s)
		g.emitLine(fmt.Sprintf("@.str.%d = private unnamed_addr constant [%d x i8] c\"%s\"", i, len(s), escaped))
	}
	if len(g.stringConsts) > 0 {
		g.emitLine("")
	}

	// Emit runtime declarations
	g.emitRuntimeDecls()
	g.emitLine("")

	// Emit class struct types, vtable types, and compute method slots.
	g.emitClassTypes()

	// Emit globals for non-entry modules' top-level typed assignments
	for _, m := range modules {
		if m.IsEntry {
			continue
		}
		if err := g.emitModuleGlobals(m); err != nil {
			return "", err
		}
	}

	// Emit user-defined functions, module by module
	for _, m := range modules {
		g.currentMod = m.ID
		g.moduleConsts = g.buildModuleConsts(m)
		for _, stmt := range m.Program.Stmts {
			if fd, ok := stmt.(*parser.FuncDef); ok {
				if fd.Extern {
					g.emitExternDecl(fd)
					continue
				}
				if err := g.emitFuncDef(fd); err != nil {
					return "", err
				}
				g.emitLine("")
			}
		}
		// Emit class methods (including synthesized __str__/__repr__).
		for _, ct := range m.Classes {
			if err := g.emitClassMethods(ct); err != nil {
				return "", err
			}
		}
	}

	// Emit vtable globals (after methods are defined).
	for _, ct := range g.classes {
		g.emitVTable(ct)
	}

	// Emit init functions for non-entry modules (assign global values)
	for _, m := range modules {
		if m.IsEntry {
			continue
		}
		g.currentMod = m.ID
		g.moduleConsts = g.buildModuleConsts(m)
		if err := g.emitModuleInit(m); err != nil {
			return "", err
		}
	}

	// Emit main
	g.currentMod = entry.ID
	g.moduleConsts = g.buildModuleConsts(entry)
	g.vars = map[string]varInfo{}
	g.inFunction = false

	g.emitLine("define i32 @main(i32 %argc, i8** %argv) {")
	g.emitLine("entry:")
	g.emitLine("  call void @spy_init()")
	// Publish argc/argv for sys.argv() et al. The runtime keeps them as
	// globals; no cost if sys isn't imported.
	g.emitLine("  call void @spy_argv_set(i32 %argc, i8** %argv)")

	// Call each non-entry module's init in topological order
	for _, m := range modules {
		if m.IsEntry {
			continue
		}
		if moduleHasInit(m) {
			g.emitLine(fmt.Sprintf("  call void @spy_%s_init()", m.ID))
		}
	}

	// Emit entry module's top-level statements (skip funcs & imports)
	for _, stmt := range entry.Program.Stmts {
		switch stmt.(type) {
		case *parser.FuncDef, *parser.ImportStmt, *parser.FromImportStmt:
			continue
		}
		if err := g.emitStmt(stmt); err != nil {
			return "", err
		}
	}

	g.emitLine("  ret i32 0")
	g.emitLine("}")

	return g.buf.String(), nil
}

// moduleConstType returns the declared or inferred type of a top-level
// constant assignment in an imported module. Annotated assignments use the
// annotation; unannotated ones fall back to the type the checker resolved on
// the RHS expression. Returns nil if neither source yields a type.
func (g *Generator) moduleConstType(as *parser.AssignStmt) types.Type {
	if as.TypeAnn != nil {
		return g.resolveTypeAnnotation(as.TypeAnn)
	}
	if t, ok := as.Value.GetResolvedType().(types.Type); ok {
		return t
	}
	return nil
}

// buildModuleConsts computes the map of module-scope names for a module:
// its own top-level constant assignments (non-entry only) plus any
// from-imports. These are the names visible at module scope but not
// allocated on the stack.
func (g *Generator) buildModuleConsts(m *ModuleInput) map[string]varInfo {
	out := map[string]varInfo{}
	if !m.IsEntry {
		for _, stmt := range m.Program.Stmts {
			as, ok := stmt.(*parser.AssignStmt)
			if !ok {
				continue
			}
			t := g.moduleConstType(as)
			if t == nil {
				continue
			}
			out[as.Name] = varInfo{
				llvmName: fmt.Sprintf("@spy_%s_%s", m.ID, as.Name),
				typ:      t,
			}
		}
	}
	// from-imports resolve to the dep module's global
	for _, stmt := range m.Program.Stmts {
		fi, ok := stmt.(*parser.FromImportStmt)
		if !ok {
			continue
		}
		// Look for the dep's typed globals by scanning its AST — we don't
		// have easy access here; instead use the resolved type that the
		// checker stashed on a reference. But no reference to it exists
		// from this statement. So pull the type from the entry module's
		// checker env? We don't have it. Fall back to scanning: we know
		// depID, origName; typ will be pulled when first used. For now
		// we only need llvmName for the load pattern, and typ must match
		// the global's type. We store a placeholder and override when
		// emitting references. This is a limitation — documented.
		for _, n := range fi.Names {
			effective := n.Name
			if n.Alias != "" {
				effective = n.Alias
			}
			out[effective] = varInfo{
				llvmName: fmt.Sprintf("@spy_%s_%s", fi.Module, n.Name),
				typ:      nil, // resolved lazily via the ident's resolved type
			}
		}
	}
	return out
}

// emitModuleGlobals emits LLVM global declarations (zero-initialized) for each
// top-level constant assignment in a non-entry module.
func (g *Generator) emitModuleGlobals(m *ModuleInput) error {
	for _, stmt := range m.Program.Stmts {
		as, ok := stmt.(*parser.AssignStmt)
		if !ok {
			continue
		}
		t := g.moduleConstType(as)
		if t == nil {
			continue
		}
		llvmT := g.llvmType(t)
		g.emitLine(fmt.Sprintf("@spy_%s_%s = global %s %s", m.ID, as.Name, llvmT, g.zeroValue(t)))
	}
	g.emitLine("")
	return nil
}

func (g *Generator) zeroValue(t types.Type) string {
	switch t.(type) {
	case *types.IntType:
		return "0"
	case *types.FloatType:
		return "0.0"
	case *types.BoolType:
		return "0"
	case *types.StrType, *types.ListType, *types.MapType:
		return "null"
	case *types.NoneType:
		return "zeroinitializer"
	}
	return "zeroinitializer"
}

// moduleHasInit reports whether a non-entry module has any top-level
// constant assignments that need initialization at startup.
func moduleHasInit(m *ModuleInput) bool {
	for _, stmt := range m.Program.Stmts {
		if _, ok := stmt.(*parser.AssignStmt); ok {
			return true
		}
	}
	return false
}

// emitModuleInit emits a `void @spy_<mod>_init()` function body that assigns
// each of the module's top-level constants to its init value.
func (g *Generator) emitModuleInit(m *ModuleInput) error {
	if !moduleHasInit(m) {
		return nil
	}
	g.emitLine(fmt.Sprintf("define void @spy_%s_init() {", m.ID))
	g.emitLine("entry:")

	oldVars := g.vars
	oldInFunc := g.inFunction
	g.vars = map[string]varInfo{}
	g.inFunction = true

	for _, stmt := range m.Program.Stmts {
		as, ok := stmt.(*parser.AssignStmt)
		if !ok {
			continue
		}
		t := g.moduleConstType(as)
		if t == nil {
			continue
		}
		val, err := g.emitExpr(as.Value)
		if err != nil {
			return err
		}
		if valType, ok := as.Value.GetResolvedType().(types.Type); ok && valType != nil {
			val = g.castToType(val, valType, t)
		}
		llvmT := g.llvmType(t)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* @spy_%s_%s", llvmT, val, llvmT, m.ID, as.Name))
	}

	g.emitLine("  ret void")
	g.emitLine("}")
	g.emitLine("")

	g.vars = oldVars
	g.inFunction = oldInFunc
	return nil
}

func (g *Generator) emitRuntimeDecls() {
	g.emitLine("declare void @spy_init()")
	g.emitLine("declare void @spy_argv_set(i32, i8**)")
	g.emitLine("declare void @spy_print_int(i64)")
	g.emitLine("declare void @spy_print_float(double)")
	g.emitLine("declare void @spy_print_bool(i1)")
	g.emitLine("declare void @spy_print_str(i8*)")
	g.emitLine("declare void @spy_print_newline()")
	g.emitLine("declare i8* @spy_str_new(i8*, i64)")
	g.emitLine("declare i8* @spy_str_concat(i8*, i8*)")
	g.emitLine("declare i1 @spy_str_eq(i8*, i8*)")
	g.emitLine("declare i8* @spy_str_index(i8*, i64)")
	g.emitLine("declare i64 @spy_str_len(i8*)")
	g.emitLine("declare i64 @spy_str_compare(i8*, i8*)")
	g.emitLine("declare i8* @spy_list_new(i64)")
	g.emitLine("declare void @spy_list_append(i8*, i8*)")
	g.emitLine("declare i8* @spy_list_get(i8*, i64)")
	g.emitLine("declare void @spy_list_set(i8*, i64, i8*)")
	g.emitLine("declare i64 @spy_list_len(i8*)")
	g.emitLine("declare i8* @spy_map_new(i64, i64, i64)")
	g.emitLine("declare void @spy_map_set(i8*, i8*, i8*)")
	g.emitLine("declare i8* @spy_map_get(i8*, i8*)")
	g.emitLine("declare i1 @spy_map_contains(i8*, i8*)")
	g.emitLine("declare i64 @spy_map_len(i8*)")
	g.emitLine("declare i8* @spy_int_to_str(i64)")
	g.emitLine("declare i8* @spy_float_to_str(double)")
	g.emitLine("declare i8* @spy_bool_to_str(i1)")
	g.emitLine("declare i64 @spy_int_pow(i64, i64)")
	g.emitLine("declare double @llvm.pow.f64(double, double)")
	g.emitLine("declare double @llvm.floor.f64(double)")
	g.emitLine("declare i8* @spy_instance_new(i64)")
	g.emitLine("declare i8* @spy_bytearray_new(i64)")
	g.emitLine("declare i8* @spy_bytearray_from_bytes(i8*)")
	g.emitLine("declare i64 @spy_bytearray_get(i8*, i64)")
	g.emitLine("declare void @spy_bytearray_set(i8*, i64, i64)")
	g.emitLine("declare void @spy_bytearray_append(i8*, i64)")
	g.emitLine("declare i8* @spy_bytearray_to_bytes(i8*)")
	g.emitLine("declare i64 @spy_bytearray_len(i8*)")
	// Exception ABI. setjmp is declared as returns_twice so LLVM does not
	// hoist loads across the call. We call the C library setjmp directly —
	// not the llvm.eh.sjlj.setjmp intrinsic, which has different semantics.
	g.emitLine("declare i32 @setjmp(i8*) returns_twice")
	g.emitLine("declare void @spy_exc_push(i8*)")
	g.emitLine("declare void @spy_exc_pop()")
	g.emitLine("declare i8* @spy_exc_current()")
	g.emitLine("declare void @spy_exc_clear()")
	g.emitLine("declare void @spy_exc_throw(i8*)")
	g.emitLine("declare void @spy_exc_rethrow()")
}

// emitExternDecl emits an LLVM `declare` for an @extern function. The symbol
// is fd.ExternSymbol if set, else the default mangling spy_<module>_<name>.
// Declarations are deduplicated by symbol to stay within LLVM's one-declare-
// per-symbol rule when multiple modules bind to the same C function.
func (g *Generator) emitExternDecl(fd *parser.FuncDef) {
	sym := fd.ExternSymbol
	if sym == "" {
		sym = fmt.Sprintf("spy_%s_%s", g.currentMod, fd.Name)
	}
	if g.declaredExterns[sym] {
		return
	}
	g.declaredExterns[sym] = true
	retType := g.getResolvedType(fd)
	retLLVM := g.llvmType(retType)
	params := []string{}
	for _, p := range fd.Params {
		pType := g.resolveTypeAnnotation(p.TypeAnn)
		params = append(params, g.llvmType(pType))
	}
	g.emitLine(fmt.Sprintf("declare %s @%s(%s)", retLLVM, sym, strings.Join(params, ", ")))
}

func (g *Generator) emitFuncDef(fd *parser.FuncDef) error {
	retType := g.getResolvedType(fd)
	retLLVM := g.llvmType(retType)
	params := []string{}
	for _, p := range fd.Params {
		pType := g.resolveTypeAnnotation(p.TypeAnn)
		params = append(params, fmt.Sprintf("%s %%%s", g.llvmType(pType), p.Name))
	}

	g.emitLine(fmt.Sprintf("define %s @spy_%s_%s(%s) {", retLLVM, g.currentMod, fd.Name, strings.Join(params, ", ")))
	g.emitLine("entry:")

	// Save state
	oldVars := g.vars
	oldInFunc := g.inFunction
	oldRet := g.currentReturnType
	oldRetLLVM := g.currentReturnLLVMType
	g.vars = make(map[string]varInfo)
	g.inFunction = true
	g.currentReturnType = retType
	g.currentReturnLLVMType = retLLVM

	// Alloca for params
	for _, p := range fd.Params {
		pType := g.resolveTypeAnnotation(p.TypeAnn)
		llvmT := g.llvmType(pType)
		allocaName := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", allocaName, llvmT))
		g.emitLine(fmt.Sprintf("  store %s %%%s, %s* %s", llvmT, p.Name, llvmT, allocaName))
		g.vars[p.Name] = varInfo{llvmName: allocaName, typ: pType}
	}

	for _, stmt := range fd.Body {
		if err := g.emitStmt(stmt); err != nil {
			return err
		}
	}

	// Ensure the trailing basic block has a terminator. A dangling label —
	// e.g. the `try.end` emitted by a try whose body always returns through
	// a finally — would otherwise leave the block unterminated and make the
	// function invalid IR.
	g.terminateOpenBlock(retLLVM)

	g.emitLine("}")

	g.vars = oldVars
	g.inFunction = oldInFunc
	g.currentReturnType = oldRet
	g.currentReturnLLVMType = oldRetLLVM
	return nil
}

func (g *Generator) getResolvedType(fd *parser.FuncDef) types.Type {
	if fd.ReturnType == nil {
		return &types.NoneType{}
	}
	return g.resolveTypeAnnotation(fd.ReturnType)
}

func (g *Generator) resolveTypeAnnotation(ann *parser.TypeAnnotation) types.Type {
	switch ann.Name {
	case "int":
		return &types.IntType{}
	case "float":
		return &types.FloatType{}
	case "bool":
		return &types.BoolType{}
	case "str":
		return &types.StrType{}
	case "bytes":
		return &types.BytesType{}
	case "bytearray":
		return &types.BytearrayType{}
	case "None":
		return &types.NoneType{}
	case "list":
		if len(ann.Params) == 1 {
			return &types.ListType{Elem: g.resolveTypeAnnotation(ann.Params[0])}
		}
	case "map":
		if len(ann.Params) == 2 {
			return &types.MapType{Key: g.resolveTypeAnnotation(ann.Params[0]), Value: g.resolveTypeAnnotation(ann.Params[1])}
		}
	case "tuple":
		elems := make([]types.Type, len(ann.Params))
		for i, p := range ann.Params {
			elems[i] = g.resolveTypeAnnotation(p)
		}
		return &types.TupleType{Elements: elems}
	}
	if ct, ok := g.classByName[ann.Name]; ok {
		return &types.InstanceType{Class: ct}
	}
	return &types.NoneType{}
}

func (g *Generator) emitStmt(stmt parser.Stmt) error {
	switch s := stmt.(type) {
	case *parser.ExprStmt:
		return g.emitExprStmt(s)
	case *parser.AssignStmt:
		return g.emitAssignStmt(s)
	case *parser.AugAssignStmt:
		return g.emitAugAssignStmt(s)
	case *parser.IndexAssignStmt:
		return g.emitIndexAssignStmt(s)
	case *parser.MultiAssignStmt:
		return g.emitMultiAssignStmt(s)
	case *parser.AttrAssignStmt:
		return g.emitAttrAssignStmt(s)
	case *parser.IfStmt:
		return g.emitIfStmt(s)
	case *parser.WhileStmt:
		return g.emitWhileStmt(s)
	case *parser.ForStmt:
		return g.emitForStmt(s)
	case *parser.ReturnStmt:
		return g.emitReturnStmt(s)
	case *parser.ClassDef:
		// Class definitions are emitted by emitClassMethods earlier; nothing
		// to do at statement emission time.
		return nil
	case *parser.BreakStmt:
		if len(g.breakLabels) == 0 {
			return fmt.Errorf("break outside loop")
		}
		if g.routeExit(breakKind, "", "") {
			g.emitLine(fmt.Sprintf("%s:", g.newLabel("after.break")))
			return nil
		}
		g.emitLine(fmt.Sprintf("  br label %%%s", g.breakLabels[len(g.breakLabels)-1]))
		g.emitLine(fmt.Sprintf("%s:", g.newLabel("after.break")))
		return nil
	case *parser.ContinueStmt:
		if len(g.continueLabels) == 0 {
			return fmt.Errorf("continue outside loop")
		}
		if g.routeExit(continueKind, "", "") {
			g.emitLine(fmt.Sprintf("%s:", g.newLabel("after.continue")))
			return nil
		}
		g.emitLine(fmt.Sprintf("  br label %%%s", g.continueLabels[len(g.continueLabels)-1]))
		g.emitLine(fmt.Sprintf("%s:", g.newLabel("after.continue")))
		return nil
	case *parser.TryStmt:
		return g.emitTryStmt(s)
	case *parser.RaiseStmt:
		return g.emitRaiseStmt(s)
	default:
		return fmt.Errorf("unknown statement type: %T", stmt)
	}
}

func (g *Generator) emitExprStmt(s *parser.ExprStmt) error {
	// Handle print specially
	if call, ok := s.Expr.(*parser.CallExpr); ok {
		if ident, ok := call.Func.(*parser.IdentExpr); ok && ident.Name == "print" {
			return g.emitPrintCall(call)
		}
		// List/dict-style methods like list.append() — only for primitive
		// collection receivers. Class method calls and super() calls flow
		// through the general CallExpr path so they get proper dispatch.
		if attr, ok := call.Func.(*parser.AttrExpr); ok {
			if _, isSuper := attr.Object.(*parser.SuperExpr); !isSuper {
				if _, isInst := attr.Object.GetResolvedType().(*types.InstanceType); !isInst {
					if _, isMod := attr.Object.GetResolvedType().(*types.ModuleType); !isMod {
						return g.emitMethodCall(attr, call.Args)
					}
				}
			}
		}
	}
	_, err := g.emitExpr(s.Expr)
	return err
}

func (g *Generator) emitPrintCall(call *parser.CallExpr) error {
	for i, arg := range call.Args {
		if i > 0 {
			// Print space separator - must create a spy_str (length-prefixed)
			spaceIdx := g.getStringIndex(" ")
			spaceTmp := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_new(i8* getelementptr ([%d x i8], [%d x i8]* @.str.%d, i64 0, i64 0), i64 %d)",
				spaceTmp, 1, 1, spaceIdx, 1))
			g.emitLine(fmt.Sprintf("  call void @spy_print_str(i8* %s)", spaceTmp))
		}
		val, err := g.emitExpr(arg)
		if err != nil {
			return err
		}
		argType := arg.GetResolvedType().(types.Type)
		switch t := argType.(type) {
		case *types.IntType:
			g.emitLine(fmt.Sprintf("  call void @spy_print_int(i64 %s)", val))
		case *types.FloatType:
			g.emitLine(fmt.Sprintf("  call void @spy_print_float(double %s)", val))
		case *types.BoolType:
			g.emitLine(fmt.Sprintf("  call void @spy_print_bool(i1 %s)", val))
		case *types.StrType, *types.BytesType:
			g.emitLine(fmt.Sprintf("  call void @spy_print_str(i8* %s)", val))
		case *types.InstanceType:
			g.printInstance(val, t)
		}
	}
	g.emitLine("  call void @spy_print_newline()")
	return nil
}

func (g *Generator) emitMethodCall(attr *parser.AttrExpr, args []parser.Expr) error {
	objVal, err := g.emitExpr(attr.Object)
	if err != nil {
		return err
	}
	objType := attr.Object.GetResolvedType().(types.Type)

	if lt, ok := objType.(*types.ListType); ok && attr.Attr == "append" {
		if len(args) != 1 {
			return fmt.Errorf("append takes 1 argument")
		}
		argVal, err := g.emitExpr(args[0])
		if err != nil {
			return err
		}
		// Store value to a temp alloca, then pass pointer
		elemLLVM := g.llvmType(lt.Elem)
		tmpAlloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", tmpAlloca, elemLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, argVal, elemLLVM, tmpAlloca))
		tmpCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", tmpCast, elemLLVM, tmpAlloca))
		g.emitLine(fmt.Sprintf("  call void @spy_list_append(i8* %s, i8* %s)", objVal, tmpCast))
		return nil
	}

	if _, ok := objType.(*types.BytearrayType); ok && attr.Attr == "append" {
		if len(args) != 1 {
			return fmt.Errorf("append takes 1 argument")
		}
		argVal, err := g.emitExpr(args[0])
		if err != nil {
			return err
		}
		g.emitLine(fmt.Sprintf("  call void @spy_bytearray_append(i8* %s, i64 %s)", objVal, argVal))
		return nil
	}

	return fmt.Errorf("unknown method: %s.%s", objType, attr.Attr)
}

func (g *Generator) emitAssignStmt(s *parser.AssignStmt) error {
	val, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}
	valType, _ := s.Value.GetResolvedType().(types.Type)

	var varType types.Type
	if s.TypeAnn != nil {
		varType = g.resolveTypeAnnotation(s.TypeAnn)
	} else if info, ok := g.vars[s.Name]; ok {
		// Reassignment
		llvmT := g.llvmType(info.typ)
		if valType != nil {
			val = g.castToType(val, valType, info.typ)
		}
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", llvmT, val, llvmT, info.llvmName))
		return nil
	} else {
		// First binding with inferred type (no annotation).
		if valType == nil {
			return fmt.Errorf("cannot infer type of %s", s.Name)
		}
		varType = valType
	}

	if valType != nil {
		val = g.castToType(val, valType, varType)
	}
	llvmT := g.llvmType(varType)
	allocaName := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca %s", allocaName, llvmT))
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", llvmT, val, llvmT, allocaName))
	g.vars[s.Name] = varInfo{llvmName: allocaName, typ: varType}

	return nil
}

// emitMultiAssignStmt emits `a, b = expr`. For each name, GEP into the tuple
// struct, load the element, and either reuse the existing alloca (for
// reassignment) or create a new one (for first binding).
func (g *Generator) emitMultiAssignStmt(s *parser.MultiAssignStmt) error {
	rhs, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}
	tt, ok := s.Value.GetResolvedType().(*types.TupleType)
	if !ok {
		return fmt.Errorf("multi-assign RHS not tuple-typed")
	}
	structTy := g.tupleStructType(tt)
	for i, name := range s.Names {
		elemType := tt.Elements[i]
		elemLLVM := g.llvmType(elemType)
		slot := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = getelementptr %s, %s* %s, i32 0, i32 %d",
			slot, structTy, structTy, rhs, i))
		v := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", v, elemLLVM, elemLLVM, slot))

		if info, already := g.vars[name]; already {
			g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, v, elemLLVM, info.llvmName))
			continue
		}
		alloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", alloca, elemLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, v, elemLLVM, alloca))
		g.vars[name] = varInfo{llvmName: alloca, typ: elemType}
	}
	return nil
}

func (g *Generator) emitAugAssignStmt(s *parser.AugAssignStmt) error {
	info, ok := g.vars[s.Name]
	if !ok {
		return fmt.Errorf("undefined variable: %s", s.Name)
	}

	llvmT := g.llvmType(info.typ)
	curVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", curVal, llvmT, llvmT, info.llvmName))

	rhsVal, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}

	result := g.newTmp()
	switch info.typ.(type) {
	case *types.IntType:
		switch s.Op {
		case "+":
			g.emitLine(fmt.Sprintf("  %s = add i64 %s, %s", result, curVal, rhsVal))
		case "-":
			g.emitLine(fmt.Sprintf("  %s = sub i64 %s, %s", result, curVal, rhsVal))
		case "*":
			g.emitLine(fmt.Sprintf("  %s = mul i64 %s, %s", result, curVal, rhsVal))
		case "/":
			g.emitIntDivZeroCheck(rhsVal, "/")
			g.emitLine(fmt.Sprintf("  %s = sdiv i64 %s, %s", result, curVal, rhsVal))
		case "//":
			g.emitIntDivZeroCheck(rhsVal, "//")
			g.emitLine(fmt.Sprintf("  %s = sdiv i64 %s, %s", result, curVal, rhsVal))
		case "%":
			g.emitIntDivZeroCheck(rhsVal, "%")
			g.emitLine(fmt.Sprintf("  %s = srem i64 %s, %s", result, curVal, rhsVal))
		case "**":
			g.emitLine(fmt.Sprintf("  %s = call i64 @spy_int_pow(i64 %s, i64 %s)", result, curVal, rhsVal))
		case "&":
			g.emitLine(fmt.Sprintf("  %s = and i64 %s, %s", result, curVal, rhsVal))
		case "|":
			g.emitLine(fmt.Sprintf("  %s = or i64 %s, %s", result, curVal, rhsVal))
		case "^":
			g.emitLine(fmt.Sprintf("  %s = xor i64 %s, %s", result, curVal, rhsVal))
		case "<<":
			g.emitLine(fmt.Sprintf("  %s = shl i64 %s, %s", result, curVal, rhsVal))
		case ">>":
			g.emitLine(fmt.Sprintf("  %s = ashr i64 %s, %s", result, curVal, rhsVal))
		}
	case *types.FloatType:
		switch s.Op {
		case "+":
			g.emitLine(fmt.Sprintf("  %s = fadd double %s, %s", result, curVal, rhsVal))
		case "-":
			g.emitLine(fmt.Sprintf("  %s = fsub double %s, %s", result, curVal, rhsVal))
		case "*":
			g.emitLine(fmt.Sprintf("  %s = fmul double %s, %s", result, curVal, rhsVal))
		case "/":
			g.emitFloatDivZeroCheck(rhsVal, "/")
			g.emitLine(fmt.Sprintf("  %s = fdiv double %s, %s", result, curVal, rhsVal))
		}
	}

	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", llvmT, result, llvmT, info.llvmName))
	return nil
}

func (g *Generator) emitIndexAssignStmt(s *parser.IndexAssignStmt) error {
	objVal, err := g.emitExpr(s.Object)
	if err != nil {
		return err
	}
	idxVal, err := g.emitExpr(s.Index)
	if err != nil {
		return err
	}
	valVal, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}

	objType := s.Object.GetResolvedType().(types.Type)

	switch t := objType.(type) {
	case *types.ListType:
		elemLLVM := g.llvmType(t.Elem)
		tmpAlloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", tmpAlloca, elemLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, valVal, elemLLVM, tmpAlloca))
		tmpCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", tmpCast, elemLLVM, tmpAlloca))
		g.emitLine(fmt.Sprintf("  call void @spy_list_set(i8* %s, i64 %s, i8* %s)", objVal, idxVal, tmpCast))

	case *types.MapType:
		keyLLVM := g.llvmType(t.Key)
		valLLVM := g.llvmType(t.Value)
		keyAlloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", keyAlloca, keyLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", keyLLVM, idxVal, keyLLVM, keyAlloca))
		keyCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", keyCast, keyLLVM, keyAlloca))
		valAlloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", valAlloca, valLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", valLLVM, valVal, valLLVM, valAlloca))
		valCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", valCast, valLLVM, valAlloca))
		g.emitLine(fmt.Sprintf("  call void @spy_map_set(i8* %s, i8* %s, i8* %s)", objVal, keyCast, valCast))

	case *types.BytearrayType:
		_ = t
		g.emitLine(fmt.Sprintf("  call void @spy_bytearray_set(i8* %s, i64 %s, i64 %s)", objVal, idxVal, valVal))
	}

	return nil
}

func (g *Generator) emitAttrAssignStmt(s *parser.AttrAssignStmt) error {
	objVal, err := g.emitExpr(s.Object)
	if err != nil {
		return err
	}
	valVal, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}
	objType := s.Object.GetResolvedType().(types.Type)
	inst, ok := objType.(*types.InstanceType)
	if !ok {
		return fmt.Errorf("cannot set attribute on %s", objType)
	}
	idx, ok := inst.Class.FieldIdx[s.Attr]
	if !ok {
		return fmt.Errorf("%s has no field %s", inst.Class.Name, s.Attr)
	}
	field := inst.Class.Fields[idx]
	fieldLLVM := g.llvmType(field.Type)

	// Cast value to field type if needed (subclass upcast).
	valType := s.Value.GetResolvedType().(types.Type)
	valVal = g.castToType(valVal, valType, field.Type)

	fieldPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %%Class.%s* %s, i32 0, i32 %d",
		fieldPtr, inst.Class.Name, inst.Class.Name, objVal, idx+1))
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", fieldLLVM, valVal, fieldLLVM, fieldPtr))
	return nil
}

// castToType upcasts `val` (of type `fromT`) to `toT` with a bitcast if both
// are instance pointer types and fromT is a subclass of toT. Returns the value
// unchanged otherwise.
func (g *Generator) castToType(val string, fromT, toT types.Type) string {
	fi, ok := fromT.(*types.InstanceType)
	if !ok {
		return val
	}
	ti, ok := toT.(*types.InstanceType)
	if !ok {
		return val
	}
	if fi.Class == ti.Class {
		return val
	}
	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %s %s to %s",
		result, g.llvmType(fromT), val, g.llvmType(toT)))
	return result
}

func (g *Generator) emitIfStmt(s *parser.IfStmt) error {
	condVal, err := g.emitExpr(s.Condition)
	if err != nil {
		return err
	}

	thenLabel := g.newLabel("if.then")
	endLabel := g.newLabel("if.end")

	var elseLabel string
	if len(s.Elifs) > 0 {
		elseLabel = g.newLabel("if.elif.0")
	} else if s.ElseBody != nil {
		elseLabel = g.newLabel("if.else")
	} else {
		elseLabel = endLabel
	}

	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", condVal, thenLabel, elseLabel))

	// Then block
	g.emitLine(fmt.Sprintf("%s:", thenLabel))
	for _, stmt := range s.Body {
		if err := g.emitStmt(stmt); err != nil {
			return err
		}
	}
	g.emitLine(fmt.Sprintf("  br label %%%s", endLabel))

	// Elif blocks
	for i, elif := range s.Elifs {
		currentLabel := elseLabel
		if i > 0 {
			currentLabel = g.newLabel(fmt.Sprintf("if.elif.%d", i))
		}
		_ = currentLabel

		g.emitLine(fmt.Sprintf("%s:", elseLabel))
		elifCond, err := g.emitExpr(elif.Condition)
		if err != nil {
			return err
		}

		elifThen := g.newLabel(fmt.Sprintf("elif.%d.then", i))
		if i+1 < len(s.Elifs) {
			elseLabel = g.newLabel(fmt.Sprintf("if.elif.%d", i+1))
		} else if s.ElseBody != nil {
			elseLabel = g.newLabel("if.else")
		} else {
			elseLabel = endLabel
		}

		g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", elifCond, elifThen, elseLabel))
		g.emitLine(fmt.Sprintf("%s:", elifThen))
		for _, stmt := range elif.Body {
			if err := g.emitStmt(stmt); err != nil {
				return err
			}
		}
		g.emitLine(fmt.Sprintf("  br label %%%s", endLabel))
	}

	// Else block
	if s.ElseBody != nil {
		g.emitLine(fmt.Sprintf("%s:", elseLabel))
		for _, stmt := range s.ElseBody {
			if err := g.emitStmt(stmt); err != nil {
				return err
			}
		}
		g.emitLine(fmt.Sprintf("  br label %%%s", endLabel))
	}

	g.emitLine(fmt.Sprintf("%s:", endLabel))
	return nil
}

func (g *Generator) emitWhileStmt(s *parser.WhileStmt) error {
	condLabel := g.newLabel("while.cond")
	bodyLabel := g.newLabel("while.body")
	endLabel := g.newLabel("while.end")

	g.breakLabels = append(g.breakLabels, endLabel)
	g.continueLabels = append(g.continueLabels, condLabel)

	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))
	g.emitLine(fmt.Sprintf("%s:", condLabel))

	condVal, err := g.emitExpr(s.Condition)
	if err != nil {
		return err
	}
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", condVal, bodyLabel, endLabel))

	g.emitLine(fmt.Sprintf("%s:", bodyLabel))
	for _, stmt := range s.Body {
		if err := g.emitStmt(stmt); err != nil {
			return err
		}
	}
	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))

	g.emitLine(fmt.Sprintf("%s:", endLabel))

	g.breakLabels = g.breakLabels[:len(g.breakLabels)-1]
	g.continueLabels = g.continueLabels[:len(g.continueLabels)-1]
	return nil
}

func (g *Generator) emitForStmt(s *parser.ForStmt) error {
	// Check if this is a range-based for loop
	if call, ok := s.Iter.(*parser.CallExpr); ok {
		if ident, ok := call.Func.(*parser.IdentExpr); ok && ident.Name == "range" {
			return g.emitForRange(s, call)
		}
	}

	// For-over-collection
	return g.emitForCollection(s)
}

func (g *Generator) emitForRange(s *parser.ForStmt, rangeCall *parser.CallExpr) error {
	var startVal, stopVal, stepVal string

	switch len(rangeCall.Args) {
	case 1:
		startVal = "0"
		var err error
		stopVal, err = g.emitExpr(rangeCall.Args[0])
		if err != nil {
			return err
		}
		stepVal = "1"
	case 2:
		var err error
		startVal, err = g.emitExpr(rangeCall.Args[0])
		if err != nil {
			return err
		}
		stopVal, err = g.emitExpr(rangeCall.Args[1])
		if err != nil {
			return err
		}
		stepVal = "1"
	case 3:
		var err error
		startVal, err = g.emitExpr(rangeCall.Args[0])
		if err != nil {
			return err
		}
		stopVal, err = g.emitExpr(rangeCall.Args[1])
		if err != nil {
			return err
		}
		stepVal, err = g.emitExpr(rangeCall.Args[2])
		if err != nil {
			return err
		}
	}

	// Alloca for loop var
	loopVar := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca i64", loopVar))
	g.emitLine(fmt.Sprintf("  store i64 %s, i64* %s", startVal, loopVar))
	g.vars[s.VarName] = varInfo{llvmName: loopVar, typ: &types.IntType{}}

	condLabel := g.newLabel("for.cond")
	bodyLabel := g.newLabel("for.body")
	incLabel := g.newLabel("for.inc")
	endLabel := g.newLabel("for.end")

	g.breakLabels = append(g.breakLabels, endLabel)
	g.continueLabels = append(g.continueLabels, incLabel)

	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))
	g.emitLine(fmt.Sprintf("%s:", condLabel))

	curVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", curVal, loopVar))
	cmpResult := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp slt i64 %s, %s", cmpResult, curVal, stopVal))
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmpResult, bodyLabel, endLabel))

	g.emitLine(fmt.Sprintf("%s:", bodyLabel))
	for _, stmt := range s.Body {
		if err := g.emitStmt(stmt); err != nil {
			return err
		}
	}
	g.emitLine(fmt.Sprintf("  br label %%%s", incLabel))

	g.emitLine(fmt.Sprintf("%s:", incLabel))
	curVal2 := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", curVal2, loopVar))
	nextVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = add i64 %s, %s", nextVal, curVal2, stepVal))
	g.emitLine(fmt.Sprintf("  store i64 %s, i64* %s", nextVal, loopVar))
	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))

	g.emitLine(fmt.Sprintf("%s:", endLabel))

	g.breakLabels = g.breakLabels[:len(g.breakLabels)-1]
	g.continueLabels = g.continueLabels[:len(g.continueLabels)-1]
	return nil
}

func (g *Generator) emitForCollection(s *parser.ForStmt) error {
	collVal, err := g.emitExpr(s.Iter)
	if err != nil {
		return err
	}

	iterType := s.Iter.GetResolvedType().(types.Type)

	switch t := iterType.(type) {
	case *types.ListType:
		return g.emitForList(s, collVal, t)
	case *types.StrType:
		return g.emitForStr(s, collVal)
	}

	return fmt.Errorf("cannot iterate over %s", iterType)
}

func (g *Generator) emitForList(s *parser.ForStmt, listVal string, lt *types.ListType) error {
	// Get list length
	lenVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i64 @spy_list_len(i8* %s)", lenVal, listVal))

	// Index variable
	idxVar := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca i64", idxVar))
	g.emitLine(fmt.Sprintf("  store i64 0, i64* %s", idxVar))

	// Loop variable alloca
	elemLLVM := g.llvmType(lt.Elem)
	loopVar := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca %s", loopVar, elemLLVM))
	g.vars[s.VarName] = varInfo{llvmName: loopVar, typ: lt.Elem}

	condLabel := g.newLabel("for.cond")
	bodyLabel := g.newLabel("for.body")
	incLabel := g.newLabel("for.inc")
	endLabel := g.newLabel("for.end")

	g.breakLabels = append(g.breakLabels, endLabel)
	g.continueLabels = append(g.continueLabels, incLabel)

	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))
	g.emitLine(fmt.Sprintf("%s:", condLabel))

	curIdx := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", curIdx, idxVar))
	cmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp slt i64 %s, %s", cmp, curIdx, lenVal))
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, bodyLabel, endLabel))

	g.emitLine(fmt.Sprintf("%s:", bodyLabel))
	// Get element
	elemPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_list_get(i8* %s, i64 %s)", elemPtr, listVal, curIdx))
	elemCast := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", elemCast, elemPtr, elemLLVM))
	elemVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", elemVal, elemLLVM, elemLLVM, elemCast))
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, elemVal, elemLLVM, loopVar))

	for _, stmt := range s.Body {
		if err := g.emitStmt(stmt); err != nil {
			return err
		}
	}
	g.emitLine(fmt.Sprintf("  br label %%%s", incLabel))

	g.emitLine(fmt.Sprintf("%s:", incLabel))
	curIdx2 := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", curIdx2, idxVar))
	nextIdx := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = add i64 %s, 1", nextIdx, curIdx2))
	g.emitLine(fmt.Sprintf("  store i64 %s, i64* %s", nextIdx, idxVar))
	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))

	g.emitLine(fmt.Sprintf("%s:", endLabel))

	g.breakLabels = g.breakLabels[:len(g.breakLabels)-1]
	g.continueLabels = g.continueLabels[:len(g.continueLabels)-1]
	return nil
}

func (g *Generator) emitForStr(s *parser.ForStmt, strVal string) error {
	lenVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i64 @spy_str_len(i8* %s)", lenVal, strVal))

	idxVar := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca i64", idxVar))
	g.emitLine(fmt.Sprintf("  store i64 0, i64* %s", idxVar))

	loopVar := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca i8*", loopVar))
	g.vars[s.VarName] = varInfo{llvmName: loopVar, typ: &types.StrType{}}

	condLabel := g.newLabel("for.cond")
	bodyLabel := g.newLabel("for.body")
	incLabel := g.newLabel("for.inc")
	endLabel := g.newLabel("for.end")

	g.breakLabels = append(g.breakLabels, endLabel)
	g.continueLabels = append(g.continueLabels, incLabel)

	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))
	g.emitLine(fmt.Sprintf("%s:", condLabel))

	curIdx := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", curIdx, idxVar))
	cmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp slt i64 %s, %s", cmp, curIdx, lenVal))
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, bodyLabel, endLabel))

	g.emitLine(fmt.Sprintf("%s:", bodyLabel))
	charVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_index(i8* %s, i64 %s)", charVal, strVal, curIdx))
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", charVal, loopVar))

	for _, stmt := range s.Body {
		if err := g.emitStmt(stmt); err != nil {
			return err
		}
	}
	g.emitLine(fmt.Sprintf("  br label %%%s", incLabel))

	g.emitLine(fmt.Sprintf("%s:", incLabel))
	curIdx2 := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", curIdx2, idxVar))
	nextIdx := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = add i64 %s, 1", nextIdx, curIdx2))
	g.emitLine(fmt.Sprintf("  store i64 %s, i64* %s", nextIdx, idxVar))
	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))

	g.emitLine(fmt.Sprintf("%s:", endLabel))

	g.breakLabels = g.breakLabels[:len(g.breakLabels)-1]
	g.continueLabels = g.continueLabels[:len(g.continueLabels)-1]
	return nil
}

func (g *Generator) emitReturnStmt(s *parser.ReturnStmt) error {
	if s.Value == nil {
		if g.routeExit(returnKind, "", "void") {
			g.emitLine(fmt.Sprintf("%s:", g.newLabel("after.return")))
			return nil
		}
		g.emitLine("  ret void")
		return nil
	}

	val, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}

	valType := s.Value.GetResolvedType().(types.Type)
	retLLVM := g.currentReturnLLVMType
	if g.currentReturnLLVMType != "" && g.currentReturnType != nil {
		val = g.castToType(val, valType, g.currentReturnType)
	} else {
		retLLVM = g.llvmType(valType)
	}
	if g.routeExit(returnKind, val, retLLVM) {
		g.emitLine(fmt.Sprintf("%s:", g.newLabel("after.return")))
		return nil
	}
	g.emitLine(fmt.Sprintf("  ret %s %s", retLLVM, val))
	return nil
}

// emitExpr returns the LLVM SSA value name for the expression result
func (g *Generator) emitExpr(expr parser.Expr) (string, error) {
	switch e := expr.(type) {
	case *parser.IntLit:
		return fmt.Sprintf("%d", e.Value), nil

	case *parser.FloatLit:
		// Emit as LLVM's exact hex form so the literal round-trips bit-for-bit.
		// `%e` would round to 7 significant digits and lose precision for values
		// like math.pi or DBL_MAX.
		return fmt.Sprintf("0x%016X", math.Float64bits(e.Value)), nil

	case *parser.BoolLit:
		if e.Value {
			return "1", nil
		}
		return "0", nil

	case *parser.StrLit:
		return g.emitStrLit(e)
	case *parser.BytesLit:
		return g.emitBytesLit(e)

	case *parser.NoneLit:
		return "void", nil

	case *parser.IdentExpr:
		// Class names are not proper values — they are only used as call
		// targets or isinstance() args, which are handled elsewhere. Return
		// an undef placeholder to satisfy the type system.
		if _, isClass := e.GetResolvedType().(*types.ClassType); isClass {
			return "undef", nil
		}
		if info, ok := g.vars[e.Name]; ok {
			llvmT := g.llvmType(info.typ)
			result := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", result, llvmT, llvmT, info.llvmName))
			return result, nil
		}
		if info, ok := g.moduleConsts[e.Name]; ok {
			// Type may be nil for from-imports; fall back to the expr's resolved type.
			t := info.typ
			if t == nil {
				t, _ = e.GetResolvedType().(types.Type)
			}
			if _, isFunc := t.(*types.FuncType); isFunc {
				// from-imported function used as a value — emit its address
				return info.llvmName, nil
			}
			llvmT := g.llvmType(t)
			result := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", result, llvmT, llvmT, info.llvmName))
			return result, nil
		}
		// Function name used as a value
		if ft, ok := e.GetResolvedType().(*types.FuncType); ok {
			mod := ft.DefinedIn
			if mod == "" {
				mod = g.currentMod
			}
			return fmt.Sprintf("@spy_%s_%s", mod, e.Name), nil
		}
		return fmt.Sprintf("@spy_%s_%s", g.currentMod, e.Name), nil

	case *parser.BinaryExpr:
		return g.emitBinaryExpr(e)

	case *parser.UnaryExpr:
		return g.emitUnaryExpr(e)

	case *parser.CallExpr:
		return g.emitCallExpr(e)

	case *parser.IndexExpr:
		return g.emitIndexExpr(e)

	case *parser.AttrExpr:
		// Module member access (e.g., foo.PI) — load from the global.
		if modT, ok := e.Object.GetResolvedType().(*types.ModuleType); ok {
			t, _ := e.GetResolvedType().(types.Type)
			if _, isFunc := t.(*types.FuncType); isFunc {
				return fmt.Sprintf("@spy_%s_%s", modT.ID, e.Attr), nil
			}
			llvmT := g.llvmType(t)
			result := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = load %s, %s* @spy_%s_%s", result, llvmT, llvmT, modT.ID, e.Attr))
			return result, nil
		}
		// Instance field access.
		if inst, ok := e.Object.GetResolvedType().(*types.InstanceType); ok {
			if idx, ok := inst.Class.FieldIdx[e.Attr]; ok {
				objVal, err := g.emitExpr(e.Object)
				if err != nil {
					return "", err
				}
				field := inst.Class.Fields[idx]
				fieldLLVM := g.llvmType(field.Type)
				fieldPtr := g.newTmp()
				g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %%Class.%s* %s, i32 0, i32 %d",
					fieldPtr, inst.Class.Name, inst.Class.Name, objVal, idx+1))
				result := g.newTmp()
				g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", result, fieldLLVM, fieldLLVM, fieldPtr))
				return result, nil
			}
			return "", fmt.Errorf("instance method %s.%s used as a value is not supported", inst.Class.Name, e.Attr)
		}
		// Fallback for method-call preamble: return the object
		return g.emitExpr(e.Object)

	case *parser.ListLit:
		return g.emitListLit(e)

	case *parser.MapLit:
		return g.emitMapLit(e)

	case *parser.TupleLit:
		return g.emitTupleLit(e)

	case *parser.SuperExpr:
		return "", fmt.Errorf("bare super() is not a value; use super().method(...)")

	default:
		return "", fmt.Errorf("unknown expression type: %T", expr)
	}
}

func (g *Generator) emitStrLit(e *parser.StrLit) (string, error) {
	idx := g.getStringIndex(e.Value)
	tmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_new(i8* getelementptr ([%d x i8], [%d x i8]* @.str.%d, i64 0, i64 0), i64 %d)",
		tmp, len(e.Value), len(e.Value), idx, len(e.Value)))
	return tmp, nil
}

// emitBytesLit uses the same runtime layout as strings: a length-prefixed
// heap-allocated buffer. The distinction between str and bytes exists only in
// the type system.
func (g *Generator) emitBytesLit(e *parser.BytesLit) (string, error) {
	idx := g.getStringIndex(e.Value)
	tmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_new(i8* getelementptr ([%d x i8], [%d x i8]* @.str.%d, i64 0, i64 0), i64 %d)",
		tmp, len(e.Value), len(e.Value), idx, len(e.Value)))
	return tmp, nil
}

func (g *Generator) emitBinaryExpr(e *parser.BinaryExpr) (string, error) {
	// Handle short-circuit for and/or
	if e.Op == "and" || e.Op == "or" {
		return g.emitShortCircuit(e)
	}

	leftVal, err := g.emitExpr(e.Left)
	if err != nil {
		return "", err
	}
	rightVal, err := g.emitExpr(e.Right)
	if err != nil {
		return "", err
	}

	leftType := e.Left.GetResolvedType().(types.Type)

	// Operator overloading on class instances: dispatch to the dunder through
	// the vtable.
	if inst, ok := leftType.(*types.InstanceType); ok {
		dunder := binaryOpDunder(e.Op)
		if dunder == "" {
			return "", fmt.Errorf("operator %s not supported on %s", e.Op, inst.Class.Name)
		}
		sig, ok := inst.Class.Methods[dunder]
		if !ok {
			return "", fmt.Errorf("%s has no method %s", inst.Class.Name, dunder)
		}
		rightT := e.Right.GetResolvedType().(types.Type)
		if len(sig.Params) == 1 {
			rightVal = g.castToType(rightVal, rightT, sig.Params[0])
			rightT = sig.Params[0]
		}
		return g.emitVirtualCall(leftVal, inst.Class, dunder, []string{rightVal}, []types.Type{rightT}, sig.Return), nil
	}

	result := g.newTmp()

	switch leftType.(type) {
	case *types.IntType:
		switch e.Op {
		case "+":
			g.emitLine(fmt.Sprintf("  %s = add i64 %s, %s", result, leftVal, rightVal))
		case "-":
			g.emitLine(fmt.Sprintf("  %s = sub i64 %s, %s", result, leftVal, rightVal))
		case "*":
			g.emitLine(fmt.Sprintf("  %s = mul i64 %s, %s", result, leftVal, rightVal))
		case "/":
			g.emitIntDivZeroCheck(rightVal, "/")
			g.emitLine(fmt.Sprintf("  %s = sdiv i64 %s, %s", result, leftVal, rightVal))
		case "//":
			g.emitIntDivZeroCheck(rightVal, "//")
			g.emitLine(fmt.Sprintf("  %s = sdiv i64 %s, %s", result, leftVal, rightVal))
		case "%":
			g.emitIntDivZeroCheck(rightVal, "%")
			g.emitLine(fmt.Sprintf("  %s = srem i64 %s, %s", result, leftVal, rightVal))
		case "**":
			// Use runtime helper for integer power
			g.emitLine(fmt.Sprintf("  %s = call i64 @spy_int_pow(i64 %s, i64 %s)", result, leftVal, rightVal))
		case "==":
			g.emitLine(fmt.Sprintf("  %s = icmp eq i64 %s, %s", result, leftVal, rightVal))
		case "!=":
			g.emitLine(fmt.Sprintf("  %s = icmp ne i64 %s, %s", result, leftVal, rightVal))
		case "<":
			g.emitLine(fmt.Sprintf("  %s = icmp slt i64 %s, %s", result, leftVal, rightVal))
		case ">":
			g.emitLine(fmt.Sprintf("  %s = icmp sgt i64 %s, %s", result, leftVal, rightVal))
		case "<=":
			g.emitLine(fmt.Sprintf("  %s = icmp sle i64 %s, %s", result, leftVal, rightVal))
		case ">=":
			g.emitLine(fmt.Sprintf("  %s = icmp sge i64 %s, %s", result, leftVal, rightVal))
		case "&":
			g.emitLine(fmt.Sprintf("  %s = and i64 %s, %s", result, leftVal, rightVal))
		case "|":
			g.emitLine(fmt.Sprintf("  %s = or i64 %s, %s", result, leftVal, rightVal))
		case "^":
			g.emitLine(fmt.Sprintf("  %s = xor i64 %s, %s", result, leftVal, rightVal))
		case "<<":
			g.emitLine(fmt.Sprintf("  %s = shl i64 %s, %s", result, leftVal, rightVal))
		case ">>":
			g.emitLine(fmt.Sprintf("  %s = ashr i64 %s, %s", result, leftVal, rightVal))
		}

	case *types.FloatType:
		switch e.Op {
		case "+":
			g.emitLine(fmt.Sprintf("  %s = fadd double %s, %s", result, leftVal, rightVal))
		case "-":
			g.emitLine(fmt.Sprintf("  %s = fsub double %s, %s", result, leftVal, rightVal))
		case "*":
			g.emitLine(fmt.Sprintf("  %s = fmul double %s, %s", result, leftVal, rightVal))
		case "/":
			g.emitFloatDivZeroCheck(rightVal, "/")
			g.emitLine(fmt.Sprintf("  %s = fdiv double %s, %s", result, leftVal, rightVal))
		case "//":
			g.emitFloatDivZeroCheck(rightVal, "//")
			// Floor division for floats: fdiv then floor
			tmpDiv := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = fdiv double %s, %s", tmpDiv, leftVal, rightVal))
			g.emitLine(fmt.Sprintf("  %s = call double @llvm.floor.f64(double %s)", result, tmpDiv))
		case "%":
			g.emitFloatDivZeroCheck(rightVal, "%")
			g.emitLine(fmt.Sprintf("  %s = frem double %s, %s", result, leftVal, rightVal))
		case "**":
			g.emitLine(fmt.Sprintf("  %s = call double @llvm.pow.f64(double %s, double %s)", result, leftVal, rightVal))
		case "==":
			g.emitLine(fmt.Sprintf("  %s = fcmp oeq double %s, %s", result, leftVal, rightVal))
		case "!=":
			g.emitLine(fmt.Sprintf("  %s = fcmp one double %s, %s", result, leftVal, rightVal))
		case "<":
			g.emitLine(fmt.Sprintf("  %s = fcmp olt double %s, %s", result, leftVal, rightVal))
		case ">":
			g.emitLine(fmt.Sprintf("  %s = fcmp ogt double %s, %s", result, leftVal, rightVal))
		case "<=":
			g.emitLine(fmt.Sprintf("  %s = fcmp ole double %s, %s", result, leftVal, rightVal))
		case ">=":
			g.emitLine(fmt.Sprintf("  %s = fcmp oge double %s, %s", result, leftVal, rightVal))
		}

	case *types.StrType:
		switch e.Op {
		case "+":
			g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_concat(i8* %s, i8* %s)", result, leftVal, rightVal))
		case "==":
			g.emitLine(fmt.Sprintf("  %s = call i1 @spy_str_eq(i8* %s, i8* %s)", result, leftVal, rightVal))
		case "!=":
			eqResult := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = call i1 @spy_str_eq(i8* %s, i8* %s)", eqResult, leftVal, rightVal))
			g.emitLine(fmt.Sprintf("  %s = xor i1 %s, 1", result, eqResult))
		case "<", ">", "<=", ">=":
			cmpResult := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = call i64 @spy_str_compare(i8* %s, i8* %s)", cmpResult, leftVal, rightVal))
			switch e.Op {
			case "<":
				g.emitLine(fmt.Sprintf("  %s = icmp slt i64 %s, 0", result, cmpResult))
			case ">":
				g.emitLine(fmt.Sprintf("  %s = icmp sgt i64 %s, 0", result, cmpResult))
			case "<=":
				g.emitLine(fmt.Sprintf("  %s = icmp sle i64 %s, 0", result, cmpResult))
			case ">=":
				g.emitLine(fmt.Sprintf("  %s = icmp sge i64 %s, 0", result, cmpResult))
			}
		}

	case *types.BoolType:
		switch e.Op {
		case "==":
			g.emitLine(fmt.Sprintf("  %s = icmp eq i1 %s, %s", result, leftVal, rightVal))
		case "!=":
			g.emitLine(fmt.Sprintf("  %s = icmp ne i1 %s, %s", result, leftVal, rightVal))
		}
	}

	return result, nil
}

func (g *Generator) emitShortCircuit(e *parser.BinaryExpr) (string, error) {
	leftVal, err := g.emitExpr(e.Left)
	if err != nil {
		return "", err
	}

	rhsLabel := g.newLabel("sc.rhs")
	mergeLabel := g.newLabel("sc.merge")

	resultAlloca := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca i1", resultAlloca))

	if e.Op == "and" {
		g.emitLine(fmt.Sprintf("  store i1 0, i1* %s", resultAlloca))
		g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", leftVal, rhsLabel, mergeLabel))
	} else { // or
		g.emitLine(fmt.Sprintf("  store i1 1, i1* %s", resultAlloca))
		g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", leftVal, mergeLabel, rhsLabel))
	}

	g.emitLine(fmt.Sprintf("%s:", rhsLabel))
	rightVal, err := g.emitExpr(e.Right)
	if err != nil {
		return "", err
	}
	g.emitLine(fmt.Sprintf("  store i1 %s, i1* %s", rightVal, resultAlloca))
	g.emitLine(fmt.Sprintf("  br label %%%s", mergeLabel))

	g.emitLine(fmt.Sprintf("%s:", mergeLabel))
	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i1, i1* %s", result, resultAlloca))
	return result, nil
}

func (g *Generator) emitUnaryExpr(e *parser.UnaryExpr) (string, error) {
	operandVal, err := g.emitExpr(e.Operand)
	if err != nil {
		return "", err
	}

	operandType := e.Operand.GetResolvedType().(types.Type)

	// Instance negation via __neg__.
	if e.Op == "-" {
		if inst, ok := operandType.(*types.InstanceType); ok {
			sig, ok := inst.Class.Methods["__neg__"]
			if !ok {
				return "", fmt.Errorf("%s has no __neg__", inst.Class.Name)
			}
			return g.emitVirtualCall(operandVal, inst.Class, "__neg__", nil, nil, sig.Return), nil
		}
	}

	result := g.newTmp()

	switch e.Op {
	case "-":
		switch operandType.(type) {
		case *types.IntType:
			g.emitLine(fmt.Sprintf("  %s = sub i64 0, %s", result, operandVal))
		case *types.FloatType:
			g.emitLine(fmt.Sprintf("  %s = fneg double %s", result, operandVal))
		}
	case "not":
		g.emitLine(fmt.Sprintf("  %s = xor i1 %s, 1", result, operandVal))
	case "~":
		g.emitLine(fmt.Sprintf("  %s = xor i64 %s, -1", result, operandVal))
	}

	return result, nil
}

func (g *Generator) emitCallExpr(e *parser.CallExpr) (string, error) {
	// Handle builtin calls
	if ident, ok := e.Func.(*parser.IdentExpr); ok {
		switch ident.Name {
		case "isinstance":
			return g.emitIsInstanceCall(e)
		case "len":
			return g.emitLenCall(e)
		case "int":
			return g.emitIntConversion(e)
		case "float":
			return g.emitFloatConversion(e)
		case "str":
			return g.emitStrConversion(e)
		case "bytes":
			return g.emitBytesConversion(e)
		case "bytearray":
			return g.emitBytearrayConversion(e)
		case "print":
			// print as expression
			if err := g.emitPrintCall(e); err != nil {
				return "", err
			}
			return "void", nil
		case "range":
			// range should not be called as a standalone expression
			return "", fmt.Errorf("range() can only be used in for loops")
		}
	}

	// Method or module-function calls via attribute access
	if attr, ok := e.Func.(*parser.AttrExpr); ok {
		// module.ClassName(...) — the AttrExpr resolves to a ClassType when the
		// attribute names a class exported by the module. Route to the same
		// constructor path as a bare ClassName(...) call.
		if ct, ok := e.Func.GetResolvedType().(*types.ClassType); ok {
			return g.emitConstructorCall(ct, e.Args)
		}
		// super().method(...) — direct (non-virtual) call to base's method.
		if _, isSuper := attr.Object.(*parser.SuperExpr); isSuper {
			return g.emitSuperCall(attr, e.Args)
		}
		// Instance method call via vtable.
		if inst, isInst := attr.Object.GetResolvedType().(*types.InstanceType); isInst {
			return g.emitInstanceMethodCall(attr, inst, e.Args)
		}
		// If the receiver is a module, it's a cross-module function call.
		if modT, isMod := attr.Object.GetResolvedType().(*types.ModuleType); isMod {
			// An @extern binding with an explicit C symbol overrides the
			// default spy_<module>_<name> mangling.
			if ft, ok := e.Func.GetResolvedType().(*types.FuncType); ok && ft.ExternSymbol != "" {
				return g.emitUserCall(ft.ExternSymbol, e)
			}
			return g.emitUserCall(fmt.Sprintf("spy_%s_%s", modT.ID, attr.Attr), e)
		}
		// Otherwise it's a method (existing behavior: list.append, etc.)
		if err := g.emitMethodCall(attr, e.Args); err != nil {
			return "", err
		}
		return "void", nil
	}

	// Constructor call: the callee is an identifier whose resolved type is
	// a ClassType. Dispatch through emitConstructorCall.
	if ident, ok := e.Func.(*parser.IdentExpr); ok {
		if ct, isClass := ident.GetResolvedType().(*types.ClassType); isClass {
			return g.emitConstructorCall(ct, e.Args)
		}
	}

	// User-defined function call via plain identifier
	if ident, ok := e.Func.(*parser.IdentExpr); ok {
		// @extern function with explicit symbol bypasses all name mangling.
		if ft, ok := ident.GetResolvedType().(*types.FuncType); ok && ft.ExternSymbol != "" {
			return g.emitUserCall(ft.ExternSymbol, e)
		}
		// If this is a from-import binding (possibly aliased), use the
		// mangled name recorded in moduleConsts so aliases resolve to
		// the original symbol.
		if info, ok := g.moduleConsts[ident.Name]; ok {
			mangled := strings.TrimPrefix(info.llvmName, "@")
			return g.emitUserCall(mangled, e)
		}
		modID := g.currentMod
		if ft, ok := ident.GetResolvedType().(*types.FuncType); ok && ft.DefinedIn != "" {
			modID = ft.DefinedIn
		}
		return g.emitUserCall(fmt.Sprintf("spy_%s_%s", modID, ident.Name), e)
	}
	return g.emitUserCall(fmt.Sprintf("spy_%s_", g.currentMod), e)
}

func (g *Generator) emitUserCall(mangled string, e *parser.CallExpr) (string, error) {
	// Fetch the callee's FuncType (if known) to know the declared parameter
	// types — needed for subclass upcasts.
	var calleeSig *types.FuncType
	if ft, ok := e.Func.GetResolvedType().(*types.FuncType); ok {
		calleeSig = ft
	}

	argVals := []string{}
	argTypes := []types.Type{}
	for i, arg := range e.Args {
		val, err := g.emitExpr(arg)
		if err != nil {
			return "", err
		}
		at := arg.GetResolvedType().(types.Type)
		if calleeSig != nil && i < len(calleeSig.Params) {
			val = g.castToType(val, at, calleeSig.Params[i])
			at = calleeSig.Params[i]
		}
		argVals = append(argVals, val)
		argTypes = append(argTypes, at)
	}

	retType := e.GetResolvedType().(types.Type)
	retLLVM := g.llvmType(retType)

	args := []string{}
	for i, val := range argVals {
		args = append(args, fmt.Sprintf("%s %s", g.llvmType(argTypes[i]), val))
	}

	if _, ok := retType.(*types.NoneType); ok {
		g.emitLine(fmt.Sprintf("  call void @%s(%s)", mangled, strings.Join(args, ", ")))
		return "void", nil
	}

	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call %s @%s(%s)", result, retLLVM, mangled, strings.Join(args, ", ")))
	return result, nil
}

func (g *Generator) emitLenCall(e *parser.CallExpr) (string, error) {
	argVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}

	argType := e.Args[0].GetResolvedType().(types.Type)
	result := g.newTmp()

	switch argType.(type) {
	case *types.StrType, *types.BytesType:
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_str_len(i8* %s)", result, argVal))
	case *types.BytearrayType:
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_bytearray_len(i8* %s)", result, argVal))
	case *types.ListType:
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_list_len(i8* %s)", result, argVal))
	case *types.MapType:
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_map_len(i8* %s)", result, argVal))
	}

	return result, nil
}

func (g *Generator) emitIntConversion(e *parser.CallExpr) (string, error) {
	argVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}

	argType := e.Args[0].GetResolvedType().(types.Type)
	result := g.newTmp()

	switch argType.(type) {
	case *types.IntType:
		return argVal, nil
	case *types.FloatType:
		g.emitLine(fmt.Sprintf("  %s = fptosi double %s to i64", result, argVal))
	case *types.BoolType:
		g.emitLine(fmt.Sprintf("  %s = zext i1 %s to i64", result, argVal))
	}

	return result, nil
}

func (g *Generator) emitFloatConversion(e *parser.CallExpr) (string, error) {
	argVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}

	argType := e.Args[0].GetResolvedType().(types.Type)
	result := g.newTmp()

	switch argType.(type) {
	case *types.FloatType:
		return argVal, nil
	case *types.IntType:
		g.emitLine(fmt.Sprintf("  %s = sitofp i64 %s to double", result, argVal))
	}

	return result, nil
}

func (g *Generator) emitStrConversion(e *parser.CallExpr) (string, error) {
	argVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}

	argType := e.Args[0].GetResolvedType().(types.Type)
	result := g.newTmp()

	switch argType.(type) {
	case *types.StrType:
		return argVal, nil
	case *types.BytesType:
		// bytes and str share the [i64 len][data] layout — reinterpret, no copy.
		return argVal, nil
	case *types.IntType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_int_to_str(i64 %s)", result, argVal))
	case *types.FloatType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_float_to_str(double %s)", result, argVal))
	case *types.BoolType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_bool_to_str(i1 %s)", result, argVal))
	}

	return result, nil
}

// emitBytesConversion handles bytes(x). str/bytes share a runtime layout so
// those conversions are zero-cost; bytearray needs a copy through
// spy_bytearray_to_bytes.
func (g *Generator) emitBytesConversion(e *parser.CallExpr) (string, error) {
	argVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}
	argType := e.Args[0].GetResolvedType().(types.Type)
	if _, ok := argType.(*types.BytearrayType); ok {
		result := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_bytearray_to_bytes(i8* %s)", result, argVal))
		return result, nil
	}
	return argVal, nil
}

// emitBytearrayConversion handles bytearray(x):
//   - bytearray(int n)   -> zero-filled buffer of length n
//   - bytearray(bytes b) -> copy of b
//   - bytearray(ba)      -> shallow copy via spy_bytearray_to_bytes + from_bytes
func (g *Generator) emitBytearrayConversion(e *parser.CallExpr) (string, error) {
	argVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}
	argType := e.Args[0].GetResolvedType().(types.Type)
	result := g.newTmp()
	switch argType.(type) {
	case *types.IntType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_bytearray_new(i64 %s)", result, argVal))
	case *types.BytesType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_bytearray_from_bytes(i8* %s)", result, argVal))
	case *types.BytearrayType:
		// Round-trip bytearray -> bytes -> bytearray to produce an independent copy.
		asBytes := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_bytearray_to_bytes(i8* %s)", asBytes, argVal))
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_bytearray_from_bytes(i8* %s)", result, asBytes))
	default:
		return "", fmt.Errorf("bytearray() cannot be constructed from %s", argType)
	}
	return result, nil
}

func (g *Generator) emitIndexExpr(e *parser.IndexExpr) (string, error) {
	objVal, err := g.emitExpr(e.Object)
	if err != nil {
		return "", err
	}
	idxVal, err := g.emitExpr(e.Index)
	if err != nil {
		return "", err
	}

	objType := e.Object.GetResolvedType().(types.Type)

	switch t := objType.(type) {
	case *types.StrType:
		result := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_index(i8* %s, i64 %s)", result, objVal, idxVal))
		return result, nil

	case *types.BytesType:
		// Layout: [i64 len][data...]. Payload starts at offset sizeof(int64_t)=8.
		// bytes[i] returns int(0..255): load one byte, zero-extend to i64.
		offset := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = add i64 %s, 8", offset, idxVal))
		elemPtr := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = getelementptr i8, i8* %s, i64 %s", elemPtr, objVal, offset))
		b := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load i8, i8* %s", b, elemPtr))
		result := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = zext i8 %s to i64", result, b))
		return result, nil

	case *types.BytearrayType:
		result := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_bytearray_get(i8* %s, i64 %s)", result, objVal, idxVal))
		return result, nil

	case *types.TupleType:
		// The type checker already guaranteed the index is a constant IntLit
		// in range, so we can trust the literal value here.
		lit := e.Index.(*parser.IntLit)
		structTy := g.tupleStructType(t)
		elemLLVM := g.llvmType(t.Elements[lit.Value])
		slot := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = getelementptr %s, %s* %s, i32 0, i32 %d",
			slot, structTy, structTy, objVal, lit.Value))
		result := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", result, elemLLVM, elemLLVM, slot))
		return result, nil

	case *types.ListType:
		elemLLVM := g.llvmType(t.Elem)
		elemPtr := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_list_get(i8* %s, i64 %s)", elemPtr, objVal, idxVal))
		elemCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", elemCast, elemPtr, elemLLVM))
		result := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", result, elemLLVM, elemLLVM, elemCast))
		return result, nil

	case *types.MapType:
		keyLLVM := g.llvmType(t.Key)
		valLLVM := g.llvmType(t.Value)
		keyAlloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", keyAlloca, keyLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", keyLLVM, idxVal, keyLLVM, keyAlloca))
		keyCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", keyCast, keyLLVM, keyAlloca))
		valPtr := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_map_get(i8* %s, i8* %s)", valPtr, objVal, keyCast))
		valCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", valCast, valPtr, valLLVM))
		result := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", result, valLLVM, valLLVM, valCast))
		return result, nil
	}

	return "", fmt.Errorf("cannot index %s", objType)
}

func (g *Generator) emitListLit(e *parser.ListLit) (string, error) {
	listType := e.GetResolvedType().(*types.ListType)
	elemSize := g.typeSize(listType.Elem)

	listVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_list_new(i64 %d)", listVal, elemSize))

	elemLLVM := g.llvmType(listType.Elem)
	for _, elem := range e.Elements {
		val, err := g.emitExpr(elem)
		if err != nil {
			return "", err
		}
		// Upcast if the element's concrete type is a subclass of the list
		// element type.
		if et, ok := elem.GetResolvedType().(types.Type); ok {
			val = g.castToType(val, et, listType.Elem)
		}
		tmpAlloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", tmpAlloca, elemLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, val, elemLLVM, tmpAlloca))
		tmpCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", tmpCast, elemLLVM, tmpAlloca))
		g.emitLine(fmt.Sprintf("  call void @spy_list_append(i8* %s, i8* %s)", listVal, tmpCast))
	}

	return listVal, nil
}

func (g *Generator) emitMapLit(e *parser.MapLit) (string, error) {
	mapType := e.GetResolvedType().(*types.MapType)
	keySize := g.typeSize(mapType.Key)
	valSize := g.typeSize(mapType.Value)

	// Hash type: 0 = int, 1 = str
	hashType := 0
	if _, ok := mapType.Key.(*types.StrType); ok {
		hashType = 1
	}

	mapVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_map_new(i64 %d, i64 %d, i64 %d)", mapVal, keySize, valSize, hashType))

	keyLLVM := g.llvmType(mapType.Key)
	valLLVM := g.llvmType(mapType.Value)

	for i := range e.Keys {
		keyVal, err := g.emitExpr(e.Keys[i])
		if err != nil {
			return "", err
		}
		valVal, err := g.emitExpr(e.Values[i])
		if err != nil {
			return "", err
		}

		keyAlloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", keyAlloca, keyLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", keyLLVM, keyVal, keyLLVM, keyAlloca))
		keyCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", keyCast, keyLLVM, keyAlloca))

		valAlloca := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", valAlloca, valLLVM))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", valLLVM, valVal, valLLVM, valAlloca))
		valCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", valCast, valLLVM, valAlloca))

		g.emitLine(fmt.Sprintf("  call void @spy_map_set(i8* %s, i8* %s, i8* %s)", mapVal, keyCast, valCast))
	}

	return mapVal, nil
}

// emitTupleLit allocates a GC-managed struct, stores each element, and
// returns the pointer.
func (g *Generator) emitTupleLit(e *parser.TupleLit) (string, error) {
	tt, ok := e.GetResolvedType().(*types.TupleType)
	if !ok {
		return "", fmt.Errorf("tuple literal without resolved type")
	}
	structTy := g.tupleStructType(tt)

	// Evaluate elements left-to-right.
	vals := make([]string, len(e.Elements))
	for i, el := range e.Elements {
		v, err := g.emitExpr(el)
		if err != nil {
			return "", err
		}
		vals[i] = v
	}

	// sizeof trick: ptrtoint (GEP null, 1) gives the struct size.
	sizeTmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = ptrtoint %s* getelementptr (%s, %s* null, i32 1) to i64",
		sizeTmp, structTy, structTy, structTy))
	raw := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_instance_new(i64 %s)", raw, sizeTmp))
	ptr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", ptr, raw, structTy))

	// Store each element at its struct index.
	for i, v := range vals {
		elemLLVM := g.llvmType(tt.Elements[i])
		slot := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = getelementptr %s, %s* %s, i32 0, i32 %d",
			slot, structTy, structTy, ptr, i))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, v, elemLLVM, slot))
	}
	return ptr, nil
}

// Helper methods

func (g *Generator) llvmType(t types.Type) string {
	switch v := t.(type) {
	case *types.IntType:
		return "i64"
	case *types.FloatType:
		return "double"
	case *types.BoolType:
		return "i1"
	case *types.StrType:
		return "i8*"
	case *types.BytesType:
		return "i8*"
	case *types.BytearrayType:
		return "i8*"
	case *types.NoneType:
		return "void"
	case *types.ListType:
		return "i8*"
	case *types.MapType:
		return "i8*"
	case *types.InstanceType:
		return fmt.Sprintf("%%Class.%s*", v.Class.Name)
	case *types.TupleType:
		return g.tupleStructType(v) + "*"
	}
	return "i64"
}

// tupleStructType returns the LLVM struct type (without the pointer suffix)
// for a tuple type. E.g. tuple[int, str] -> "{i64, i8*}".
func (g *Generator) tupleStructType(t *types.TupleType) string {
	parts := make([]string, len(t.Elements))
	for i, e := range t.Elements {
		parts[i] = g.llvmType(e)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (g *Generator) typeSize(t types.Type) int {
	switch t.(type) {
	case *types.IntType:
		return 8
	case *types.FloatType:
		return 8
	case *types.BoolType:
		return 1
	case *types.StrType:
		return 8 // pointer size
	case *types.ListType:
		return 8
	case *types.MapType:
		return 8
	case *types.InstanceType:
		return 8 // pointer size
	}
	return 8
}

func (g *Generator) newTmp() string {
	g.tmpCounter++
	return fmt.Sprintf("%%t%d", g.tmpCounter)
}

// terminateOpenBlock ensures the last block emitted in the current function
// has a terminator. Inspects the tail of the buffer: if the last non-empty
// line is a `label:` with no following instruction, emits either `ret void`
// (for void-returning functions) or `unreachable`. Safe no-op otherwise.
func (g *Generator) terminateOpenBlock(retLLVM string) {
	s := g.buf.String()
	// Trim trailing whitespace/newlines and find the last line.
	end := len(s)
	for end > 0 && (s[end-1] == '\n' || s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	start := end
	for start > 0 && s[start-1] != '\n' {
		start--
	}
	last := s[start:end]
	// A label line ends in ":" and has no leading "  " (instructions are
	// indented). Also exclude comment-only lines.
	if len(last) == 0 || last[len(last)-1] != ':' {
		// Last line isn't a label. It's either a terminator (br/ret/etc.)
		// or a regular instruction. For regular instructions, we still
		// may need a terminator — but the only case where we'd emit a
		// non-terminator as the final line is if someone emitted partial
		// code; existing codegen always ends blocks with terminators. For
		// void functions, add the usual implicit `ret void`.
		if retLLVM == "void" {
			g.emitLine("  ret void")
		}
		return
	}
	if retLLVM == "void" {
		g.emitLine("  ret void")
	} else {
		g.emitLine("  unreachable")
	}
}

func (g *Generator) newLabel(prefix string) string {
	g.lblCounter++
	return fmt.Sprintf("%s.%d", prefix, g.lblCounter)
}

func (g *Generator) emitLine(line string) {
	g.buf.WriteString(line)
	g.buf.WriteString("\n")
}

func (g *Generator) collectStringsInStmt(stmt parser.Stmt) {
	switch s := stmt.(type) {
	case *parser.ExprStmt:
		g.collectStringsInExpr(s.Expr)
	case *parser.AssignStmt:
		g.collectStringsInExpr(s.Value)
	case *parser.AugAssignStmt:
		g.collectStringsInExpr(s.Value)
	case *parser.MultiAssignStmt:
		g.collectStringsInExpr(s.Value)
	case *parser.IndexAssignStmt:
		g.collectStringsInExpr(s.Object)
		g.collectStringsInExpr(s.Index)
		g.collectStringsInExpr(s.Value)
	case *parser.AttrAssignStmt:
		g.collectStringsInExpr(s.Object)
		g.collectStringsInExpr(s.Value)
	case *parser.ClassDef:
		for _, m := range s.Methods {
			for _, stmt := range m.Body {
				g.collectStringsInStmt(stmt)
			}
		}
	case *parser.IfStmt:
		g.collectStringsInExpr(s.Condition)
		for _, stmt := range s.Body {
			g.collectStringsInStmt(stmt)
		}
		for _, elif := range s.Elifs {
			g.collectStringsInExpr(elif.Condition)
			for _, stmt := range elif.Body {
				g.collectStringsInStmt(stmt)
			}
		}
		for _, stmt := range s.ElseBody {
			g.collectStringsInStmt(stmt)
		}
	case *parser.WhileStmt:
		g.collectStringsInExpr(s.Condition)
		for _, stmt := range s.Body {
			g.collectStringsInStmt(stmt)
		}
	case *parser.ForStmt:
		g.collectStringsInExpr(s.Iter)
		for _, stmt := range s.Body {
			g.collectStringsInStmt(stmt)
		}
	case *parser.FuncDef:
		for _, stmt := range s.Body {
			g.collectStringsInStmt(stmt)
		}
	case *parser.ReturnStmt:
		if s.Value != nil {
			g.collectStringsInExpr(s.Value)
		}
	case *parser.TryStmt:
		for _, stmt := range s.Body {
			g.collectStringsInStmt(stmt)
		}
		for _, ec := range s.Excepts {
			for _, stmt := range ec.Body {
				g.collectStringsInStmt(stmt)
			}
		}
		for _, stmt := range s.FinallyBody {
			g.collectStringsInStmt(stmt)
		}
	case *parser.RaiseStmt:
		g.collectStringsInExpr(s.Value)
	}
}

func (g *Generator) collectStringsInExpr(expr parser.Expr) {
	switch e := expr.(type) {
	case *parser.StrLit:
		g.addStringConst(e.Value)
	case *parser.BytesLit:
		g.addStringConst(e.Value)
	case *parser.BinaryExpr:
		g.collectStringsInExpr(e.Left)
		g.collectStringsInExpr(e.Right)
	case *parser.UnaryExpr:
		g.collectStringsInExpr(e.Operand)
	case *parser.CallExpr:
		g.collectStringsInExpr(e.Func)
		for _, arg := range e.Args {
			g.collectStringsInExpr(arg)
		}
	case *parser.IndexExpr:
		g.collectStringsInExpr(e.Object)
		g.collectStringsInExpr(e.Index)
	case *parser.AttrExpr:
		g.collectStringsInExpr(e.Object)
	case *parser.ListLit:
		for _, elem := range e.Elements {
			g.collectStringsInExpr(elem)
		}
	case *parser.MapLit:
		for _, k := range e.Keys {
			g.collectStringsInExpr(k)
		}
		for _, v := range e.Values {
			g.collectStringsInExpr(v)
		}
	case *parser.TupleLit:
		for _, el := range e.Elements {
			g.collectStringsInExpr(el)
		}
	}
}

func (g *Generator) addStringConst(s string) {
	for _, existing := range g.stringConsts {
		if existing == s {
			return
		}
	}
	g.stringConsts = append(g.stringConsts, s)
}

func (g *Generator) getStringIndex(s string) int {
	for i, existing := range g.stringConsts {
		if existing == s {
			return i
		}
	}
	// Should not happen if collectStrings was called first
	g.addStringConst(s)
	return len(g.stringConsts) - 1
}

func (g *Generator) escapeString(s string) string {
	// Iterate by byte, not rune, to support non-UTF-8 content (bytes literals
	// can contain arbitrary octets). Emit anything outside printable ASCII as
	// a hex escape so the [N x i8] length matches the emitted payload exactly.
	result := make([]byte, 0, len(s))
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b == '\\':
			result = append(result, '\\', '5', 'C')
		case b == '"':
			result = append(result, '\\', '2', '2')
		case b >= 0x20 && b < 0x7f:
			result = append(result, b)
		default:
			result = append(result, '\\', hex[b>>4], hex[b&0x0f])
		}
	}
	return string(result)
}
