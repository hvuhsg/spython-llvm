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
	// While inside a function, body emits go to funcBody and alloca
	// instructions go to funcAllocas. After the function finishes, the
	// allocas are written before the body so every alloca lives in the
	// entry block — otherwise allocas inside loops accumulate stack
	// space across iterations and cause stack overflow on hot loops.
	funcBody     *strings.Builder
	funcAllocas  *strings.Builder
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

	// Generator codegen state. Each `def` whose body contains a `yield`
	// is compiled into a synthesized class (the "gen class") implementing
	// the iterator protocol. genFuncClass maps the source FuncDef to its
	// gen class; genClassFunc is the reverse. genLayouts holds the
	// per-generator field layout used by emitGeneratorMethods.
	genFuncClass map[*parser.FuncDef]*types.ClassType
	genClassFunc map[*types.ClassType]*parser.FuncDef
	genLayouts   map[*types.ClassType]*genLayout
	// genClassSet marks classes synthesized by the generator pipeline so
	// emitClassMethods knows to skip them (we emit __iter__ and __next__
	// ourselves, with a custom state-machine body).
	genClassSet map[*types.ClassType]bool
	// genCtx is non-nil only while emitting a generator's __next__ body;
	// emitYieldStmt consults it to find the state pointer and resume
	// label table.
	genCtx *genEmitCtx

	// Closure (lambda / first-class function) codegen state. Each lambda is
	// lowered to a top-level function taking the closure environment as an
	// implicit first i8* argument. closureID assigns unique names;
	// closureTypeDefs accumulates the `%clo.N = type {...}` lines; closureJobs
	// queues the function bodies to emit after the main module body.
	closureID       int
	closureTypeDefs []string
	closureJobs     []closureJob
}

// closureJob records everything needed to emit a closure's top-level function
// after the enclosing module body has been generated. Exactly one of lam / fd
// is set: lam for a lambda (single-expression body), fd for a nested def
// (statement body).
type closureJob struct {
	id        int
	lam       *parser.LambdaExpr
	fd        *parser.FuncDef
	ft        *types.FuncType
	captures  []string
	capTypes  []types.Type
	mod       string
	modConsts map[string]varInfo
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
		genFuncClass:    map[*parser.FuncDef]*types.ClassType{},
		genClassFunc:    map[*types.ClassType]*parser.FuncDef{},
		genLayouts:      map[*types.ClassType]*genLayout{},
		genClassSet:     map[*types.ClassType]bool{},
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

	// Synthesize a class per generator function (`def` with `yield` body).
	// Must run BEFORE emitClassTypes so the gen-class struct/vtable types
	// are emitted alongside user classes.
	for _, m := range modules {
		for _, stmt := range m.Program.Stmts {
			if fd, ok := stmt.(*parser.FuncDef); ok && fd.IsGenerator {
				if err := g.registerGenerator(m.ID, fd); err != nil {
					return "", err
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
	g.addStringConst("StopIteration")
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

	// Emit globals for every module's top-level typed assignments. The entry
	// module needs them too so functions defined in it can read module-scope
	// names: without globals, those reads would dangle past main()'s stack.
	for _, m := range modules {
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
		// Emit generator class methods (__iter__ and __next__) for any
		// generator funcdefs in this module.
		for _, stmt := range m.Program.Stmts {
			if fd, ok := stmt.(*parser.FuncDef); ok && fd.IsGenerator {
				if err := g.emitGeneratorMethods(fd); err != nil {
					return "", err
				}
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
	oldBody, oldAllocas := g.beginFunc()
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
	g.endFunc(oldBody, oldAllocas)
	g.emitLine("}")

	// Emit lambda function bodies + closure environment type definitions.
	if err := g.drainClosures(); err != nil {
		return "", err
	}

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
// top-level constant assignment in the module. The same name appearing in
// multiple top-level assignments (e.g., a reassignment in entry-module code)
// emits a single global.
func (g *Generator) emitModuleGlobals(m *ModuleInput) error {
	seen := map[string]bool{}
	for _, stmt := range m.Program.Stmts {
		as, ok := stmt.(*parser.AssignStmt)
		if !ok {
			continue
		}
		if seen[as.Name] {
			continue
		}
		t := g.moduleConstType(as)
		if t == nil {
			continue
		}
		seen[as.Name] = true
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
	case *types.StrType, *types.BytesType, *types.BytearrayType, *types.ListType, *types.MapType, *types.SetType, *types.IteratorType, *types.AnyType:
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
	oldBody, oldAllocas := g.beginFunc()

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
	g.endFunc(oldBody, oldAllocas)
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
	g.emitLine("declare i8* @spy_gc_alloc(i64)")
	g.emitLine("declare i8* @spy_str_new(i8*, i64)")
	g.emitLine("declare i8* @spy_str_concat(i8*, i8*)")
	g.emitLine("declare i1 @spy_str_eq(i8*, i8*)")
	g.emitLine("declare i8* @spy_str_index(i8*, i64)")
	g.emitLine("declare i64 @spy_str_len(i8*)")
	g.emitLine("declare i64 @spy_str_compare(i8*, i8*)")
	g.emitLine("declare i8* @spy_str_upper(i8*)")
	g.emitLine("declare i8* @spy_str_lower(i8*)")
	g.emitLine("declare i8* @spy_str_capitalize(i8*)")
	g.emitLine("declare i8* @spy_str_strip(i8*)")
	g.emitLine("declare i8* @spy_str_lstrip(i8*)")
	g.emitLine("declare i8* @spy_str_rstrip(i8*)")
	g.emitLine("declare i1 @spy_str_startswith(i8*, i8*)")
	g.emitLine("declare i1 @spy_str_endswith(i8*, i8*)")
	g.emitLine("declare i64 @spy_str_find(i8*, i8*)")
	g.emitLine("declare i64 @spy_str_rfind(i8*, i8*)")
	g.emitLine("declare i64 @spy_str_count(i8*, i8*)")
	g.emitLine("declare i8* @spy_str_replace(i8*, i8*, i8*)")
	g.emitLine("declare i8* @spy_str_zfill(i8*, i64)")
	g.emitLine("declare i8* @spy_str_split(i8*, i8*)")
	g.emitLine("declare i8* @spy_str_join(i8*, i8*)")
	g.emitLine("declare i1 @spy_str_isdigit(i8*)")
	g.emitLine("declare i1 @spy_str_isalpha(i8*)")
	g.emitLine("declare i1 @spy_str_isspace(i8*)")
	g.emitLine("declare i1 @spy_str_isupper(i8*)")
	g.emitLine("declare i1 @spy_str_islower(i8*)")
	g.emitLine("declare i8* @spy_list_new(i64)")
	g.emitLine("declare void @spy_list_append(i8*, i8*)")
	g.emitLine("declare i8* @spy_list_get(i8*, i64)")
	g.emitLine("declare void @spy_list_set(i8*, i64, i8*)")
	g.emitLine("declare i64 @spy_list_len(i8*)")
	g.emitLine("declare i8* @spy_list_pop(i8*)")
	g.emitLine("declare void @spy_list_insert(i8*, i64, i8*)")
	g.emitLine("declare i64 @spy_list_index(i8*, i8*, i64)")
	g.emitLine("declare i64 @spy_list_count_elem(i8*, i8*, i64)")
	g.emitLine("declare void @spy_list_remove(i8*, i8*, i64)")
	g.emitLine("declare void @spy_list_reverse(i8*)")
	g.emitLine("declare void @spy_list_clear(i8*)")
	g.emitLine("declare void @spy_list_extend(i8*, i8*)")
	g.emitLine("declare void @spy_list_sort(i8*, i64)")
	g.emitLine("declare void @spy_list_sort_key(i8*, i8*, i64, i64, i64)")
	g.emitLine("declare i8* @spy_str_slice(i8*, i64, i64, i64, i64)")
	g.emitLine("declare i8* @spy_bytes_slice(i8*, i64, i64, i64, i64)")
	g.emitLine("declare i8* @spy_list_slice(i8*, i64, i64, i64, i64)")
	g.emitLine("declare i8* @spy_bytearray_slice(i8*, i64, i64, i64, i64)")
	g.emitLine("declare i8* @spy_map_new(i64, i64, i64)")
	g.emitLine("declare void @spy_map_set(i8*, i8*, i8*)")
	g.emitLine("declare i8* @spy_map_get(i8*, i8*)")
	g.emitLine("declare i1 @spy_map_contains(i8*, i8*)")
	g.emitLine("declare i64 @spy_map_len(i8*)")
	g.emitLine("declare void @spy_map_extend(i8*, i8*)")
	g.emitLine("declare i8* @spy_map_keys(i8*)")
	g.emitLine("declare i8* @spy_map_values(i8*)")
	g.emitLine("declare i8* @spy_map_get_or(i8*, i8*, i8*)")
	g.emitLine("declare void @spy_map_clear(i8*)")
	g.emitLine("declare i64 @spy_map_next(i8*, i64)")
	g.emitLine("declare i8* @spy_map_key_at(i8*, i64)")
	g.emitLine("declare i8* @spy_set_new(i64, i64)")
	g.emitLine("declare void @spy_set_add(i8*, i8*)")
	g.emitLine("declare i1 @spy_set_contains(i8*, i8*)")
	g.emitLine("declare void @spy_set_discard(i8*, i8*)")
	g.emitLine("declare i64 @spy_set_len(i8*)")
	g.emitLine("declare i64 @spy_set_next(i8*, i64)")
	g.emitLine("declare i8* @spy_set_key(i8*, i64)")
	g.emitLine("declare i8* @spy_int_to_str(i64)")
	g.emitLine("declare i8* @spy_float_to_str(double)")
	g.emitLine("declare i8* @spy_bool_to_str(i1)")
	g.emitLine("declare i64 @spy_str_to_int(i8*)")
	g.emitLine("declare double @spy_str_to_float(i8*)")
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
	// Any (tagged value box).
	g.emitLine("declare i8* @spy_any_none()")
	g.emitLine("declare i8* @spy_any_box_int(i64)")
	g.emitLine("declare i8* @spy_any_box_float(double)")
	g.emitLine("declare i8* @spy_any_box_bool(i1)")
	g.emitLine("declare i8* @spy_any_box_str(i8*)")
	g.emitLine("declare i8* @spy_any_box_list(i8*)")
	g.emitLine("declare i8* @spy_any_box_map(i8*)")
	g.emitLine("declare i8* @spy_any_box_bytes(i8*)")
	g.emitLine("declare i32 @spy_any_tag(i8*)")
	g.emitLine("declare i1 @spy_any_is_none(i8*)")
	g.emitLine("declare i64 @spy_any_unbox_int(i8*)")
	g.emitLine("declare double @spy_any_unbox_float(i8*)")
	g.emitLine("declare i1 @spy_any_unbox_bool(i8*)")
	g.emitLine("declare i8* @spy_any_unbox_str(i8*)")
	g.emitLine("declare i8* @spy_any_unbox_list(i8*)")
	g.emitLine("declare i8* @spy_any_unbox_map(i8*)")
	g.emitLine("declare i8* @spy_any_unbox_bytes(i8*)")
	g.emitLine("declare i8* @spy_any_to_str(i8*)")
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
	if fd.IsGenerator {
		return g.emitGeneratorFactory(fd)
	}
	retType := g.getResolvedType(fd)
	retLLVM := g.llvmType(retType)
	params := []string{}
	for _, p := range fd.Params {
		pType := g.paramRuntimeType(p)
		params = append(params, fmt.Sprintf("%s %%%s", g.llvmType(pType), p.Name))
	}

	g.emitLine(fmt.Sprintf("define %s @spy_%s_%s(%s) {", retLLVM, g.currentMod, fd.Name, strings.Join(params, ", ")))
	g.emitLine("entry:")
	oldBody, oldAllocas := g.beginFunc()

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
		pType := g.paramRuntimeType(p)
		llvmT := g.llvmType(pType)
		allocaName := g.newTmp()
		g.emitAlloca(allocaName, llvmT)
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

	g.endFunc(oldBody, oldAllocas)
	g.emitLine("}")

	g.vars = oldVars
	g.inFunction = oldInFunc
	g.currentReturnType = oldRet
	g.currentReturnLLVMType = oldRetLLVM
	return nil
}

// closureFnPtrType returns the LLVM function-pointer type for a closure with
// signature ft: `<ret> (i8*, <param types...>)*` (the i8* is the environment).
func (g *Generator) closureFnPtrType(ft *types.FuncType) string {
	parts := []string{"i8*"}
	for _, p := range ft.Params {
		parts = append(parts, g.llvmType(p))
	}
	return fmt.Sprintf("%s (%s)*", g.llvmType(ft.Return), strings.Join(parts, ", "))
}

// emitLambda emits the creation of a closure value: it allocates the
// environment struct, stores the function pointer and captured values, and
// queues the lambda's top-level function for later emission. Returns the
// closure as an i8*.
func (g *Generator) emitLambda(e *parser.LambdaExpr) (string, error) {
	ft, ok := e.GetResolvedType().(*types.FuncType)
	if !ok {
		return "", fmt.Errorf("lambda has no resolved function type")
	}
	job := closureJob{
		id: g.closureID, lam: e, ft: ft,
		captures: append([]string{}, e.Captures...),
		mod:      g.currentMod, modConsts: g.moduleConsts,
	}
	g.closureID++
	return g.emitClosureValue(job)
}

// emitClosureValue allocates a closure environment, stores the function
// pointer and captured values, queues the function body for emission, and
// returns the closure as an i8*. The environment is an untyped heap block of
// 8-byte slots: slot 0 is the function pointer, slots 1..n are the captures.
// Byte offsets (rather than a named struct type) keep the IR free of any
// type-ordering constraints; every captured scalar/pointer fits in 8 bytes.
func (g *Generator) emitClosureValue(job closureJob) (string, error) {
	job.capTypes = make([]types.Type, len(job.captures))
	for i, name := range job.captures {
		info, ok := g.vars[name]
		if !ok {
			return "", fmt.Errorf("closure captures %q which is not an in-scope local", name)
		}
		job.capTypes[i] = info.typ
	}
	g.closureJobs = append(g.closureJobs, job)

	nslots := 1 + len(job.captures)
	raw := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_gc_alloc(i64 %d)", raw, nslots*8))

	fpp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to i8**", fpp, raw))
	fnPtrTy := g.closureFnPtrType(job.ft)
	g.emitLine(fmt.Sprintf("  store i8* bitcast (%s @spy_lambda_%d to i8*), i8** %s", fnPtrTy, job.id, fpp))

	for i, name := range job.captures {
		info := g.vars[name]
		ct := g.llvmType(job.capTypes[i])
		cv := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", cv, ct, ct, info.llvmName))
		slot := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = getelementptr i8, i8* %s, i64 %d", slot, raw, (i+1)*8))
		slotT := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", slotT, slot, ct))
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", ct, cv, ct, slotT))
	}
	return raw, nil
}

// emitNestedFuncDef lowers a `def` nested in a function: it builds a closure
// value (capturing free variables) and binds it to a local variable named
// after the function.
func (g *Generator) emitNestedFuncDef(s *parser.FuncDef) error {
	ft := &types.FuncType{Closure: true, Return: g.getResolvedType(s)}
	for _, p := range s.Params {
		ft.Params = append(ft.Params, g.resolveTypeAnnotation(p.TypeAnn))
	}
	job := closureJob{
		id: g.closureID, fd: s, ft: ft,
		captures: append([]string{}, s.Captures...),
		mod:      g.currentMod, modConsts: g.moduleConsts,
	}
	g.closureID++
	val, err := g.emitClosureValue(job)
	if err != nil {
		return err
	}
	a := g.newTmp()
	g.emitAlloca(a, "i8*")
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", val, a))
	g.vars[s.Name] = varInfo{llvmName: a, typ: ft}
	return nil
}

// emitClosureCall lowers `f(args)` where f evaluates to a closure value.
func (g *Generator) emitClosureCall(e *parser.CallExpr, ft *types.FuncType) (string, error) {
	cloVal, err := g.emitExpr(e.Func)
	if err != nil {
		return "", err
	}
	// Load the function pointer from environment field 0.
	fpp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to i8**", fpp, cloVal))
	fp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", fp, fpp))
	fn := g.newTmp()
	fnPtrTy := g.closureFnPtrType(ft)
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s", fn, fp, fnPtrTy))

	argStrs := []string{fmt.Sprintf("i8* %s", cloVal)}
	for i, a := range e.Args {
		av, err := g.emitExpr(a)
		if err != nil {
			return "", err
		}
		at, _ := a.GetResolvedType().(types.Type)
		if at != nil && i < len(ft.Params) {
			av = g.castToType(av, at, ft.Params[i])
		}
		pllvm := "i8*"
		if i < len(ft.Params) {
			pllvm = g.llvmType(ft.Params[i])
		}
		argStrs = append(argStrs, fmt.Sprintf("%s %s", pllvm, av))
	}

	retLLVM := g.llvmType(ft.Return)
	if _, isNone := ft.Return.(*types.NoneType); isNone || retLLVM == "void" {
		g.emitLine(fmt.Sprintf("  call void %s(%s)", fn, strings.Join(argStrs, ", ")))
		return "void", nil
	}
	r := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call %s %s(%s)", r, retLLVM, fn, strings.Join(argStrs, ", ")))
	return r, nil
}

// emitClosureFunction emits the top-level function body for a queued lambda.
func (g *Generator) emitClosureFunction(job closureJob) error {
	ft := job.ft
	retLLVM := g.llvmType(ft.Return)

	// Parameter names come from the lambda or the nested def.
	paramNames := make([]string, len(ft.Params))
	if job.lam != nil {
		copy(paramNames, job.lam.Params)
	} else {
		for i, p := range job.fd.Params {
			paramNames[i] = p.Name
		}
	}

	params := []string{"i8* %clo"}
	for i, pt := range ft.Params {
		params = append(params, fmt.Sprintf("%s %%p%d", g.llvmType(pt), i))
	}
	g.emitLine(fmt.Sprintf("define %s @spy_lambda_%d(%s) {", retLLVM, job.id, strings.Join(params, ", ")))
	g.emitLine("entry:")
	oldBody, oldAllocas := g.beginFunc()
	oldVars := g.vars
	oldInFunc := g.inFunction
	oldRet := g.currentReturnType
	oldRetLLVM := g.currentReturnLLVMType
	g.vars = map[string]varInfo{}
	g.inFunction = true
	g.currentReturnType = ft.Return
	g.currentReturnLLVMType = retLLVM

	// Bind parameters (alloca + store) under the closure's parameter names.
	for i, pt := range ft.Params {
		llvmT := g.llvmType(pt)
		a := g.newTmp()
		g.emitAlloca(a, llvmT)
		g.emitLine(fmt.Sprintf("  store %s %%p%d, %s* %s", llvmT, i, llvmT, a))
		g.vars[paramNames[i]] = varInfo{llvmName: a, typ: pt}
	}

	// Bind captures: load from the environment (8-byte slots 1..n) into
	// fresh allocas so the body's variable loads work uniformly.
	for i, name := range job.captures {
		ct := g.llvmType(job.capTypes[i])
		slot := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = getelementptr i8, i8* %%clo, i64 %d", slot, (i+1)*8))
		slotT := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", slotT, slot, ct))
		v := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", v, ct, ct, slotT))
		a := g.newTmp()
		g.emitAlloca(a, ct)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", ct, v, ct, a))
		g.vars[name] = varInfo{llvmName: a, typ: job.capTypes[i]}
	}

	if job.fd != nil {
		// Statement body (nested def): emit statements, then guarantee the
		// trailing block has a terminator (mirrors emitFuncDef).
		for _, stmt := range job.fd.Body {
			if err := g.emitStmt(stmt); err != nil {
				return err
			}
		}
		g.terminateOpenBlock(retLLVM)
	} else {
		bodyVal, err := g.emitExpr(job.lam.Body)
		if err != nil {
			return err
		}
		if _, isNone := ft.Return.(*types.NoneType); isNone || retLLVM == "void" {
			g.emitLine("  ret void")
		} else {
			bt, _ := job.lam.Body.GetResolvedType().(types.Type)
			if bt != nil {
				bodyVal = g.castToType(bodyVal, bt, ft.Return)
			}
			g.emitLine(fmt.Sprintf("  ret %s %s", retLLVM, bodyVal))
		}
	}

	g.endFunc(oldBody, oldAllocas)
	g.emitLine("}")
	g.vars = oldVars
	g.inFunction = oldInFunc
	g.currentReturnType = oldRet
	g.currentReturnLLVMType = oldRetLLVM
	return nil
}

// drainClosures emits all queued lambda functions (and any lambdas they in
// turn contain), followed by the closure environment type definitions.
func (g *Generator) drainClosures() error {
	for i := 0; i < len(g.closureJobs); i++ {
		job := g.closureJobs[i]
		g.currentMod = job.mod
		g.moduleConsts = job.modConsts
		if err := g.emitClosureFunction(job); err != nil {
			return err
		}
		g.emitLine("")
	}
	return nil
}

func (g *Generator) getResolvedType(fd *parser.FuncDef) types.Type {
	if fd.ReturnType == nil {
		return &types.NoneType{}
	}
	return g.resolveTypeAnnotation(fd.ReturnType)
}

// paramRuntimeType returns the type of a function parameter as seen inside
// the body. Regular params resolve their annotation; *args becomes list[T];
// **kwargs becomes map[str, T].
func (g *Generator) paramRuntimeType(p parser.FuncParam) types.Type {
	switch p.Kind {
	case parser.ParamVarArgs:
		return &types.ListType{Elem: g.resolveTypeAnnotation(p.TypeAnn)}
	case parser.ParamKwargs:
		return &types.MapType{Key: &types.StrType{}, Value: g.resolveTypeAnnotation(p.TypeAnn)}
	}
	return g.resolveTypeAnnotation(p.TypeAnn)
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
	case "Any":
		return &types.AnyType{}
	case "list":
		if len(ann.Params) == 1 {
			return &types.ListType{Elem: g.resolveTypeAnnotation(ann.Params[0])}
		}
	case "map":
		if len(ann.Params) == 2 {
			return &types.MapType{Key: g.resolveTypeAnnotation(ann.Params[0]), Value: g.resolveTypeAnnotation(ann.Params[1])}
		}
	case "set":
		if len(ann.Params) == 1 {
			return &types.SetType{Elem: g.resolveTypeAnnotation(ann.Params[0])}
		}
	case "tuple":
		elems := make([]types.Type, len(ann.Params))
		for i, p := range ann.Params {
			elems[i] = g.resolveTypeAnnotation(p)
		}
		return &types.TupleType{Elements: elems}
	case "Iterator":
		if len(ann.Params) == 1 {
			return &types.IteratorType{Elem: g.resolveTypeAnnotation(ann.Params[0])}
		}
	case "Callable":
		params := make([]types.Type, len(ann.CallableArgs))
		for i, a := range ann.CallableArgs {
			params[i] = g.resolveTypeAnnotation(a)
		}
		var ret types.Type = &types.NoneType{}
		if ann.CallableRet != nil {
			ret = g.resolveTypeAnnotation(ann.CallableRet)
		}
		return &types.FuncType{Params: params, Return: ret, Closure: true}
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
	case *parser.YieldStmt:
		return g.emitYieldStmt(s)
	case *parser.FuncDef:
		// Only nested functions reach emitStmt (top-level defs are emitted by
		// GenerateAll). They become closure values bound to a local.
		if s.IsClosure {
			return g.emitNestedFuncDef(s)
		}
		return nil
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
						// list.sort(key=..., reverse=...) routes through the
						// closure-aware sort; plain sort() falls through.
						if attr.Attr == "sort" && len(call.Kwargs) > 0 {
							if lt, isList := attr.Object.GetResolvedType().(*types.ListType); isList {
								_, err := g.emitListSortKey(lt, attr, call)
								return err
							}
						}
						_, err := g.emitMethodCall(attr, call.Args)
						return err
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
		case *types.AnyType:
			s := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_to_str(i8* %s)", s, val))
			g.emitLine(fmt.Sprintf("  call void @spy_print_str(i8* %s)", s))
		case *types.InstanceType:
			g.printInstance(val, t)
		}
	}
	g.emitLine("  call void @spy_print_newline()")
	return nil
}

// emitMethodCall lowers a method call on a built-in container receiver. It
// returns the result SSA value ("void" for None-returning mutators) so the
// same path serves both statement and expression positions.
func (g *Generator) emitMethodCall(attr *parser.AttrExpr, args []parser.Expr) (string, error) {
	objVal, err := g.emitExpr(attr.Object)
	if err != nil {
		return "", err
	}
	objType := attr.Object.GetResolvedType().(types.Type)

	if lt, ok := objType.(*types.ListType); ok && attr.Attr == "append" {
		if len(args) != 1 {
			return "", fmt.Errorf("append takes 1 argument")
		}
		argVal, err := g.emitExpr(args[0])
		if err != nil {
			return "", err
		}
		// Store value to a temp alloca, then pass pointer
		elemLLVM := g.llvmType(lt.Elem)
		tmpAlloca := g.newTmp()
		g.emitAlloca(tmpAlloca, elemLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, argVal, elemLLVM, tmpAlloca))
		tmpCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", tmpCast, elemLLVM, tmpAlloca))
		g.emitLine(fmt.Sprintf("  call void @spy_list_append(i8* %s, i8* %s)", objVal, tmpCast))
		return "void", nil
	}

	if _, ok := objType.(*types.BytearrayType); ok && attr.Attr == "append" {
		if len(args) != 1 {
			return "", fmt.Errorf("append takes 1 argument")
		}
		argVal, err := g.emitExpr(args[0])
		if err != nil {
			return "", err
		}
		g.emitLine(fmt.Sprintf("  call void @spy_bytearray_append(i8* %s, i64 %s)", objVal, argVal))
		return "void", nil
	}

	if st, ok := objType.(*types.SetType); ok && (attr.Attr == "add" || attr.Attr == "discard") {
		if len(args) != 1 {
			return "", fmt.Errorf("%s takes 1 argument", attr.Attr)
		}
		argVal, err := g.emitExpr(args[0])
		if err != nil {
			return "", err
		}
		if at, ok := args[0].GetResolvedType().(types.Type); ok {
			argVal = g.castToType(argVal, at, st.Elem)
		}
		elemLLVM := g.llvmType(st.Elem)
		tmpAlloca := g.newTmp()
		g.emitAlloca(tmpAlloca, elemLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, argVal, elemLLVM, tmpAlloca))
		tmpCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", tmpCast, elemLLVM, tmpAlloca))
		fn := "spy_set_add"
		if attr.Attr == "discard" {
			fn = "spy_set_discard"
		}
		g.emitLine(fmt.Sprintf("  call void @%s(i8* %s, i8* %s)", fn, objVal, tmpCast))
		return "void", nil
	}

	// String methods.
	if _, ok := objType.(*types.StrType); ok {
		return g.emitStrMethod(attr.Attr, objVal, args)
	}

	// List methods (append handled above).
	if lt, ok := objType.(*types.ListType); ok {
		return g.emitListMethod(lt, attr.Attr, objVal, args)
	}

	// Dict methods.
	if mt, ok := objType.(*types.MapType); ok {
		return g.emitMapMethod(mt, attr.Attr, objVal, args)
	}

	return "", fmt.Errorf("unknown method: %s.%s", objType, attr.Attr)
}

// emitMapMethod lowers dict.<method>(args). objVal is the receiver's i8*.
func (g *Generator) emitMapMethod(mt *types.MapType, name string, objVal string, args []parser.Expr) (string, error) {
	switch name {
	case "keys":
		r := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_map_keys(i8* %s)", r, objVal))
		return r, nil
	case "values":
		r := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_map_values(i8* %s)", r, objVal))
		return r, nil
	case "clear":
		g.emitLine(fmt.Sprintf("  call void @spy_map_clear(i8* %s)", objVal))
		return "void", nil
	case "update":
		other, err := g.emitExpr(args[0])
		if err != nil {
			return "", err
		}
		g.emitLine(fmt.Sprintf("  call void @spy_map_extend(i8* %s, i8* %s)", objVal, other))
		return "void", nil
	case "get":
		keyPtr, err := g.elemArgToPtr(args[0], mt.Key)
		if err != nil {
			return "", err
		}
		defPtr, err := g.elemArgToPtr(args[1], mt.Value)
		if err != nil {
			return "", err
		}
		valLLVM := g.llvmType(mt.Value)
		p := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_map_get_or(i8* %s, i8* %s, i8* %s)", p, objVal, keyPtr, defPtr))
		cast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", cast, p, valLLVM))
		r := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", r, valLLVM, valLLVM, cast))
		return r, nil
	}
	return "", fmt.Errorf("unknown dict method: %s", name)
}

// elemArgToPtr evaluates arg, casts it to elemType, stores it in a fresh
// alloca, and returns an i8* pointing at it (the calling convention the
// runtime container primitives expect for by-value elements).
func (g *Generator) elemArgToPtr(arg parser.Expr, elemType types.Type) (string, error) {
	v, err := g.emitExpr(arg)
	if err != nil {
		return "", err
	}
	if at, ok := arg.GetResolvedType().(types.Type); ok {
		v = g.castToType(v, at, elemType)
	}
	elemLLVM := g.llvmType(elemType)
	a := g.newTmp()
	g.emitAlloca(a, elemLLVM)
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, v, elemLLVM, a))
	cast := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", cast, elemLLVM, a))
	return cast, nil
}

// sortKindOf maps a sortable element/key type to the runtime comparison kind:
// 0 = int64, 1 = double, 2 = str pointer.
func sortKindOf(t types.Type) string {
	switch t.(type) {
	case *types.FloatType:
		return "1"
	case *types.StrType:
		return "2"
	}
	return "0"
}

// emitListSortKey lowers list.sort(key=..., reverse=...) into a call to the
// closure-aware runtime sort. `key` (when present) is a closure value passed
// as i8*; a null pointer means "sort by the element itself". The runtime
// computes keys by invoking the closure, then performs a stable sort.
func (g *Generator) emitListSortKey(lt *types.ListType, attr *parser.AttrExpr, e *parser.CallExpr) (string, error) {
	objVal, err := g.emitExpr(attr.Object)
	if err != nil {
		return "", err
	}
	elemKind := sortKindOf(lt.Elem)
	keyVal := "null"
	keyKind := elemKind
	reverseVal := "0"
	for _, kw := range e.Kwargs {
		switch kw.Name {
		case "key":
			kv, err := g.emitExpr(kw.Value)
			if err != nil {
				return "", err
			}
			keyVal = kv
			if ft, ok := kw.Value.GetResolvedType().(*types.FuncType); ok {
				keyKind = sortKindOf(ft.Return)
			}
		case "reverse":
			rv, err := g.emitExpr(kw.Value)
			if err != nil {
				return "", err
			}
			z := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = zext i1 %s to i64", z, rv))
			reverseVal = z
		}
	}
	g.emitLine(fmt.Sprintf("  call void @spy_list_sort_key(i8* %s, i8* %s, i64 %s, i64 %s, i64 %s)",
		objVal, keyVal, elemKind, keyKind, reverseVal))
	return "void", nil
}

// emitListMethod lowers list.<method>(args). objVal is the receiver's i8*.
func (g *Generator) emitListMethod(lt *types.ListType, name string, objVal string, args []parser.Expr) (string, error) {
	elemLLVM := g.llvmType(lt.Elem)
	// Equality kind: 1 = element is a str/bytes pointer (compare by value).
	eqKind := "0"
	switch lt.Elem.(type) {
	case *types.StrType, *types.BytesType:
		eqKind = "1"
	}
	switch name {
	case "pop":
		p := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_list_pop(i8* %s)", p, objVal))
		cast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", cast, p, elemLLVM))
		r := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", r, elemLLVM, elemLLVM, cast))
		return r, nil
	case "reverse":
		g.emitLine(fmt.Sprintf("  call void @spy_list_reverse(i8* %s)", objVal))
		return "void", nil
	case "clear":
		g.emitLine(fmt.Sprintf("  call void @spy_list_clear(i8* %s)", objVal))
		return "void", nil
	case "insert":
		idxVal, err := g.emitExpr(args[0])
		if err != nil {
			return "", err
		}
		ptr, err := g.elemArgToPtr(args[1], lt.Elem)
		if err != nil {
			return "", err
		}
		g.emitLine(fmt.Sprintf("  call void @spy_list_insert(i8* %s, i64 %s, i8* %s)", objVal, idxVal, ptr))
		return "void", nil
	case "remove":
		ptr, err := g.elemArgToPtr(args[0], lt.Elem)
		if err != nil {
			return "", err
		}
		g.emitLine(fmt.Sprintf("  call void @spy_list_remove(i8* %s, i8* %s, i64 %s)", objVal, ptr, eqKind))
		return "void", nil
	case "index", "count":
		ptr, err := g.elemArgToPtr(args[0], lt.Elem)
		if err != nil {
			return "", err
		}
		fn := "spy_list_index"
		if name == "count" {
			fn = "spy_list_count_elem"
		}
		r := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i64 @%s(i8* %s, i8* %s, i64 %s)", r, fn, objVal, ptr, eqKind))
		return r, nil
	case "extend":
		other, err := g.emitExpr(args[0])
		if err != nil {
			return "", err
		}
		g.emitLine(fmt.Sprintf("  call void @spy_list_extend(i8* %s, i8* %s)", objVal, other))
		return "void", nil
	case "sort":
		sortKind := "0"
		switch lt.Elem.(type) {
		case *types.FloatType:
			sortKind = "1"
		case *types.StrType:
			sortKind = "2"
		}
		g.emitLine(fmt.Sprintf("  call void @spy_list_sort(i8* %s, i64 %s)", objVal, sortKind))
		return "void", nil
	}
	return "", fmt.Errorf("unknown list method: %s", name)
}

// emitStrMethod lowers str.<method>(args). objVal is the receiver's i8*.
func (g *Generator) emitStrMethod(name string, objVal string, args []parser.Expr) (string, error) {
	// Group 0: no-arg, str-returning.
	switch name {
	case "upper", "lower", "capitalize", "strip", "lstrip", "rstrip":
		r := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_%s(i8* %s)", r, name, objVal))
		return r, nil
	case "isdigit", "isalpha", "isspace", "isupper", "islower":
		r := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i1 @spy_str_%s(i8* %s)", r, name, objVal))
		return r, nil
	}
	// Remaining methods take one or two arguments.
	argVals := make([]string, len(args))
	for i, a := range args {
		v, err := g.emitExpr(a)
		if err != nil {
			return "", err
		}
		argVals[i] = v
	}
	r := g.newTmp()
	switch name {
	case "startswith", "endswith":
		g.emitLine(fmt.Sprintf("  %s = call i1 @spy_str_%s(i8* %s, i8* %s)", r, name, objVal, argVals[0]))
	case "find", "rfind", "count":
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_str_%s(i8* %s, i8* %s)", r, name, objVal, argVals[0]))
	case "replace":
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_replace(i8* %s, i8* %s, i8* %s)", r, objVal, argVals[0], argVals[1]))
	case "zfill":
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_zfill(i8* %s, i64 %s)", r, objVal, argVals[0]))
	case "split":
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_split(i8* %s, i8* %s)", r, objVal, argVals[0]))
	case "join":
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_join(i8* %s, i8* %s)", r, objVal, argVals[0]))
	default:
		return "", fmt.Errorf("unknown str method: %s", name)
	}
	return r, nil
}

func (g *Generator) emitAssignStmt(s *parser.AssignStmt) error {
	val, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}
	valType, _ := s.Value.GetResolvedType().(types.Type)

	// Module-level assignment to an own top-level constant: write to the
	// LLVM global so functions in this module can read it. Without this,
	// the value would live in a stack slot inside main() that's invisible
	// to other functions.
	if !g.inFunction {
		if info, ok := g.moduleConsts[s.Name]; ok && info.typ != nil {
			llvmT := g.llvmType(info.typ)
			if valType != nil {
				val = g.castToType(val, valType, info.typ)
			}
			g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", llvmT, val, llvmT, info.llvmName))
			return nil
		}
	}

	// If the name is already bound, store into the existing slot (whether
	// it's a stack alloca or a generator-state field GEP). Annotated
	// re-assignments use this path too — for generators the annotation is
	// just a human-readable echo of the type, and the binding must reuse
	// the gen-state field rather than creating a fresh stack slot.
	if info, ok := g.vars[s.Name]; ok {
		llvmT := g.llvmType(info.typ)
		if valType != nil {
			val = g.castToType(val, valType, info.typ)
		}
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", llvmT, val, llvmT, info.llvmName))
		return nil
	}
	var varType types.Type
	if s.TypeAnn != nil {
		varType = g.resolveTypeAnnotation(s.TypeAnn)
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
	g.emitAlloca(allocaName, llvmT)
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
		g.emitAlloca(alloca, elemLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, v, elemLLVM, alloca))
		g.vars[name] = varInfo{llvmName: alloca, typ: elemType}
	}
	return nil
}

func (g *Generator) emitAugAssignStmt(s *parser.AugAssignStmt) error {
	info, ok := g.vars[s.Name]
	if !ok {
		// Fall back to module-level globals (e.g., aug-assigning a module
		// constant from main()).
		if mc, mcOk := g.moduleConsts[s.Name]; mcOk && mc.typ != nil && !g.inFunction {
			info = mc
		} else {
			return fmt.Errorf("undefined variable: %s", s.Name)
		}
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
		g.emitAlloca(tmpAlloca, elemLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, valVal, elemLLVM, tmpAlloca))
		tmpCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", tmpCast, elemLLVM, tmpAlloca))
		g.emitLine(fmt.Sprintf("  call void @spy_list_set(i8* %s, i64 %s, i8* %s)", objVal, idxVal, tmpCast))

	case *types.MapType:
		keyLLVM := g.llvmType(t.Key)
		valLLVM := g.llvmType(t.Value)
		keyAlloca := g.newTmp()
		g.emitAlloca(keyAlloca, keyLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", keyLLVM, idxVal, keyLLVM, keyAlloca))
		keyCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", keyCast, keyLLVM, keyAlloca))
		valAlloca := g.newTmp()
		g.emitAlloca(valAlloca, valLLVM)
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

// castToType upcasts `val` (of type `fromT`) to `toT`. Handles three cases:
//   - InstanceType subclass -> superclass: emits a bitcast on the pointer.
//   - Any concrete T -> AnyType: emits the matching spy_any_box_<T> call so
//     the container or target slot stores a tagged box rather than the raw
//     scalar/pointer.
//   - Otherwise the value passes through unchanged (identity types).
func (g *Generator) castToType(val string, fromT, toT types.Type) string {
	// T -> Any: box the value via spy_any_box_<kind>.
	if _, ok := toT.(*types.AnyType); ok {
		if _, alreadyAny := fromT.(*types.AnyType); alreadyAny {
			return val
		}
		return g.emitAnyBox(val, fromT)
	}
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

// emitAnyBuiltin lowers a call to one of the any_* builtins (the family
// declared in types.AnyBuiltinName) into the corresponding runtime call.
// The type checker has already validated arity and that the argument is Any
// (except for any_none which takes no args), so this only emits.
func (g *Generator) emitAnyBuiltin(name string, e *parser.CallExpr) (string, error) {
	if name == "any_none" {
		out := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_none()", out))
		return out, nil
	}
	if len(e.Args) != 1 {
		return "", fmt.Errorf("%s takes 1 argument", name)
	}
	argVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}
	out := g.newTmp()
	switch name {
	case "any_int":
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_any_unbox_int(i8* %s)", out, argVal))
	case "any_float":
		g.emitLine(fmt.Sprintf("  %s = call double @spy_any_unbox_float(i8* %s)", out, argVal))
	case "any_str":
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_unbox_str(i8* %s)", out, argVal))
	case "any_bool":
		g.emitLine(fmt.Sprintf("  %s = call i1 @spy_any_unbox_bool(i8* %s)", out, argVal))
	case "any_bytes":
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_unbox_bytes(i8* %s)", out, argVal))
	case "any_list":
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_unbox_list(i8* %s)", out, argVal))
	case "any_dict":
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_unbox_map(i8* %s)", out, argVal))
	case "any_tag":
		t32 := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i32 @spy_any_tag(i8* %s)", t32, argVal))
		g.emitLine(fmt.Sprintf("  %s = sext i32 %s to i64", out, t32))
	case "any_is_none":
		g.emitLine(fmt.Sprintf("  %s = call i1 @spy_any_is_none(i8* %s)", out, argVal))
	default:
		return "", fmt.Errorf("unhandled any builtin %q", name)
	}
	return out, nil
}

// emitAnyBox emits a spy_any_box_<kind> call for `val` whose static type is
// `fromT`. NoneType becomes spy_any_none(). Class instances are not yet
// boxable (no tag is reserved for them) so the function falls back to a
// no-op cast — the type checker is responsible for rejecting that.
func (g *Generator) emitAnyBox(val string, fromT types.Type) string {
	result := g.newTmp()
	switch fromT.(type) {
	case *types.IntType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_box_int(i64 %s)", result, val))
	case *types.FloatType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_box_float(double %s)", result, val))
	case *types.BoolType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_box_bool(i1 %s)", result, val))
	case *types.StrType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_box_str(i8* %s)", result, val))
	case *types.BytesType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_box_bytes(i8* %s)", result, val))
	case *types.ListType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_box_list(i8* %s)", result, val))
	case *types.MapType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_box_map(i8* %s)", result, val))
	case *types.NoneType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_any_none()", result))
	default:
		// Unknown source type — pass through. The type checker should not
		// allow this path; defensive only.
		return val
	}
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
	g.emitAlloca(loopVar, "i64")
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
	case *types.SetType:
		return g.emitForSet(s, collVal, t)
	case *types.StrType:
		return g.emitForStr(s, collVal)
	case *types.MapType:
		return g.emitForMap(s, collVal, t)
	case *types.IteratorType:
		return g.emitForIterator(s, collVal, t)
	}

	return fmt.Errorf("cannot iterate over %s", iterType)
}

// emitForMap iterates a dict, binding the loop variable to each key (CPython
// semantics). Modeled on emitForSet, using spy_map_next / spy_map_key_at.
func (g *Generator) emitForMap(s *parser.ForStmt, mapVal string, mt *types.MapType) error {
	idxVar := g.newTmp()
	g.emitAlloca(idxVar, "i64")
	g.emitLine(fmt.Sprintf("  store i64 -1, i64* %s", idxVar))

	keyLLVM := g.llvmType(mt.Key)
	loopVar := g.newTmp()
	g.emitAlloca(loopVar, keyLLVM)
	g.vars[s.VarName] = varInfo{llvmName: loopVar, typ: mt.Key}

	condLabel := g.newLabel("formap.cond")
	bodyLabel := g.newLabel("formap.body")
	incLabel := g.newLabel("formap.inc")
	endLabel := g.newLabel("formap.end")

	g.breakLabels = append(g.breakLabels, endLabel)
	g.continueLabels = append(g.continueLabels, incLabel)

	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))
	g.emitLine(fmt.Sprintf("%s:", condLabel))

	curIdx := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", curIdx, idxVar))
	nextIdx := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i64 @spy_map_next(i8* %s, i64 %s)", nextIdx, mapVal, curIdx))
	cmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp sge i64 %s, 0", cmp, nextIdx))
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, bodyLabel, endLabel))

	g.emitLine(fmt.Sprintf("%s:", bodyLabel))
	keyPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_map_key_at(i8* %s, i64 %s)", keyPtr, mapVal, nextIdx))
	keyCast := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", keyCast, keyPtr, keyLLVM))
	keyVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", keyVal, keyLLVM, keyLLVM, keyCast))
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", keyLLVM, keyVal, keyLLVM, loopVar))

	for _, stmt := range s.Body {
		if err := g.emitStmt(stmt); err != nil {
			return err
		}
	}
	g.emitLine(fmt.Sprintf("  br label %%%s", incLabel))

	g.emitLine(fmt.Sprintf("%s:", incLabel))
	g.emitLine(fmt.Sprintf("  store i64 %s, i64* %s", nextIdx, idxVar))
	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))

	g.emitLine(fmt.Sprintf("%s:", endLabel))

	g.breakLabels = g.breakLabels[:len(g.breakLabels)-1]
	g.continueLabels = g.continueLabels[:len(g.continueLabels)-1]
	return nil
}

func (g *Generator) emitForSet(s *parser.ForStmt, setVal string, st *types.SetType) error {
	idxVar := g.newTmp()
	g.emitAlloca(idxVar, "i64")
	g.emitLine(fmt.Sprintf("  store i64 0, i64* %s", idxVar))

	elemLLVM := g.llvmType(st.Elem)
	loopVar := g.newTmp()
	g.emitAlloca(loopVar, elemLLVM)
	g.vars[s.VarName] = varInfo{llvmName: loopVar, typ: st.Elem}

	condLabel := g.newLabel("forset.cond")
	bodyLabel := g.newLabel("forset.body")
	incLabel := g.newLabel("forset.inc")
	endLabel := g.newLabel("forset.end")

	g.breakLabels = append(g.breakLabels, endLabel)
	g.continueLabels = append(g.continueLabels, incLabel)

	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))
	g.emitLine(fmt.Sprintf("%s:", condLabel))

	curIdx := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", curIdx, idxVar))
	nextIdx := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i64 @spy_set_next(i8* %s, i64 %s)", nextIdx, setVal, curIdx))
	cmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp sge i64 %s, 0", cmp, nextIdx))
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, bodyLabel, endLabel))

	g.emitLine(fmt.Sprintf("%s:", bodyLabel))
	keyPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_set_key(i8* %s, i64 %s)", keyPtr, setVal, nextIdx))
	keyCast := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", keyCast, keyPtr, elemLLVM))
	keyVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", keyVal, elemLLVM, elemLLVM, keyCast))
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, keyVal, elemLLVM, loopVar))

	for _, stmt := range s.Body {
		if err := g.emitStmt(stmt); err != nil {
			return err
		}
	}
	g.emitLine(fmt.Sprintf("  br label %%%s", incLabel))

	g.emitLine(fmt.Sprintf("%s:", incLabel))
	advance := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = add i64 %s, 1", advance, nextIdx))
	g.emitLine(fmt.Sprintf("  store i64 %s, i64* %s", advance, idxVar))
	g.emitLine(fmt.Sprintf("  br label %%%s", condLabel))

	g.emitLine(fmt.Sprintf("%s:", endLabel))

	g.breakLabels = g.breakLabels[:len(g.breakLabels)-1]
	g.continueLabels = g.continueLabels[:len(g.continueLabels)-1]
	return nil
}

func (g *Generator) emitForList(s *parser.ForStmt, listVal string, lt *types.ListType) error {
	// Get list length
	lenVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i64 @spy_list_len(i8* %s)", lenVal, listVal))

	// Index variable
	idxVar := g.newTmp()
	g.emitAlloca(idxVar, "i64")
	g.emitLine(fmt.Sprintf("  store i64 0, i64* %s", idxVar))

	// Loop variable alloca
	elemLLVM := g.llvmType(lt.Elem)
	loopVar := g.newTmp()
	g.emitAlloca(loopVar, elemLLVM)
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
	g.emitAlloca(idxVar, "i64")
	g.emitLine(fmt.Sprintf("  store i64 0, i64* %s", idxVar))

	loopVar := g.newTmp()
	g.emitAlloca(loopVar, "i8*")
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
		if g.genCtx != nil {
			g.emitLine(fmt.Sprintf("  br label %%%s", g.genCtx.exhaustedLbl))
			g.emitLine(fmt.Sprintf("%s:", g.newLabel("after.gen.return")))
			return nil
		}
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

	case *parser.LambdaExpr:
		return g.emitLambda(e)

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
			if ft, isFunc := t.(*types.FuncType); isFunc && !ft.Closure {
				// from-imported named function used as a value — emit its address
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

	case *parser.SliceExpr:
		return g.emitSliceExpr(e)

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

	case *parser.SetLit:
		return g.emitSetLit(e)

	case *parser.TupleLit:
		return g.emitTupleLit(e)

	case *parser.SuperExpr:
		return "", fmt.Errorf("bare super() is not a value; use super().method(...)")

	default:
		return "", fmt.Errorf("unknown expression type: %T", expr)
	}
}

func (g *Generator) emitStrLit(e *parser.StrLit) (string, error) {
	return g.emitStringLiteral(e.Value), nil
}

// emitStringLiteral materializes a literal Go string as a runtime spy_str
// (i8* handle), interning the bytes via getStringIndex.
func (g *Generator) emitStringLiteral(s string) string {
	idx := g.getStringIndex(s)
	tmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_new(i8* getelementptr ([%d x i8], [%d x i8]* @.str.%d, i64 0, i64 0), i64 %d)",
		tmp, len(s), len(s), idx, len(s)))
	return tmp
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

// valueToPtr stores an already-emitted value (optionally cast from fromType
// to toType) in a fresh alloca and returns an i8* to it.
func (g *Generator) valueToPtr(val string, fromType, toType types.Type) string {
	if fromType != nil {
		val = g.castToType(val, fromType, toType)
	}
	elemLLVM := g.llvmType(toType)
	a := g.newTmp()
	g.emitAlloca(a, elemLLVM)
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, val, elemLLVM, a))
	cast := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", cast, elemLLVM, a))
	return cast
}

// emitMembership lowers `elem in container` / `elem not in container`.
func (g *Generator) emitMembership(e *parser.BinaryExpr, elemVal, containerVal string) (string, error) {
	containerType := e.Right.GetResolvedType().(types.Type)
	elemType := e.Left.GetResolvedType().(types.Type)
	neg := e.Op == "not in"
	r := g.newTmp()
	switch ct := containerType.(type) {
	case *types.StrType:
		f := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_str_find(i8* %s, i8* %s)", f, containerVal, elemVal))
		if neg {
			g.emitLine(fmt.Sprintf("  %s = icmp slt i64 %s, 0", r, f))
		} else {
			g.emitLine(fmt.Sprintf("  %s = icmp sge i64 %s, 0", r, f))
		}
		return r, nil
	case *types.ListType:
		ptr := g.valueToPtr(elemVal, elemType, ct.Elem)
		kind := "0"
		switch ct.Elem.(type) {
		case *types.StrType, *types.BytesType:
			kind = "1"
		}
		idx := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_list_index(i8* %s, i8* %s, i64 %s)", idx, containerVal, ptr, kind))
		if neg {
			g.emitLine(fmt.Sprintf("  %s = icmp slt i64 %s, 0", r, idx))
		} else {
			g.emitLine(fmt.Sprintf("  %s = icmp sge i64 %s, 0", r, idx))
		}
		return r, nil
	case *types.SetType:
		ptr := g.valueToPtr(elemVal, elemType, ct.Elem)
		c := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i1 @spy_set_contains(i8* %s, i8* %s)", c, containerVal, ptr))
		if neg {
			g.emitLine(fmt.Sprintf("  %s = xor i1 %s, 1", r, c))
			return r, nil
		}
		return c, nil
	case *types.MapType:
		ptr := g.valueToPtr(elemVal, elemType, ct.Key)
		c := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i1 @spy_map_contains(i8* %s, i8* %s)", c, containerVal, ptr))
		if neg {
			g.emitLine(fmt.Sprintf("  %s = xor i1 %s, 1", r, c))
			return r, nil
		}
		return c, nil
	}
	return "", fmt.Errorf("'in' not supported on %s", containerType)
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

	// Membership tests dispatch on the container (right operand), before
	// instance operator overloading (the element may itself be an instance).
	if e.Op == "in" || e.Op == "not in" {
		return g.emitMembership(e, leftVal, rightVal)
	}

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
		rightLLVM := g.llvmType(rightT)
		return g.emitVirtualCall(leftVal, inst.Class, dunder, []string{fmt.Sprintf("%s %s", rightLLVM, rightVal)}, []string{rightLLVM}, sig.Return), nil
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

	// bytes / bytearray share str's length-prefixed layout, so the same
	// runtime primitives (byte-wise concat / equality / lexicographic
	// compare) implement their operators correctly.
	case *types.StrType, *types.BytesType, *types.BytearrayType:
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
	g.emitAlloca(resultAlloca, "i1")

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
	// Calling a first-class closure value (lambda result, Callable-typed
	// variable/param, or any expression whose type is a closure FuncType).
	if ft, ok := e.Func.GetResolvedType().(*types.FuncType); ok && ft.Closure {
		return g.emitClosureCall(e, ft)
	}
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
		case "next":
			return g.emitNextCall(e)
		case "any_int", "any_float", "any_str", "any_bool",
			"any_bytes", "any_list", "any_dict",
			"any_tag", "any_is_none", "any_none":
			return g.emitAnyBuiltin(ident.Name, e)
		}
	}

	// Method or module-function calls via attribute access
	if attr, ok := e.Func.(*parser.AttrExpr); ok {
		// module.ClassName(...) — the AttrExpr resolves to a ClassType when the
		// attribute names a class exported by the module. Route to the same
		// constructor path as a bare ClassName(...) call.
		if ct, ok := e.Func.GetResolvedType().(*types.ClassType); ok {
			return g.emitConstructorCall(ct, e)
		}
		// super().method(...) — direct (non-virtual) call to base's method.
		if _, isSuper := attr.Object.(*parser.SuperExpr); isSuper {
			return g.emitSuperCall(attr, e)
		}
		// Instance method call via vtable.
		if inst, isInst := attr.Object.GetResolvedType().(*types.InstanceType); isInst {
			return g.emitInstanceMethodCall(attr, inst, e)
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
		// list.sort(key=..., reverse=...): handled specially because it routes
		// through a closure-aware runtime sort. Plain sort() falls through.
		if attr.Attr == "sort" && len(e.Kwargs) > 0 {
			if lt, isList := attr.Object.GetResolvedType().(*types.ListType); isList {
				return g.emitListSortKey(lt, attr, e)
			}
		}
		// Otherwise it's a built-in container method (list.append, str.upper,
		// …). emitMethodCall returns the result value ("void" for mutators).
		return g.emitMethodCall(attr, e.Args)
	}

	// Constructor call: the callee is an identifier whose resolved type is
	// a ClassType. Dispatch through emitConstructorCall.
	if ident, ok := e.Func.(*parser.IdentExpr); ok {
		if ct, isClass := ident.GetResolvedType().(*types.ClassType); isClass {
			return g.emitConstructorCall(ct, e)
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
	// types — needed for subclass upcasts and varargs/kwargs handling.
	var calleeSig *types.FuncType
	if ft, ok := e.Func.GetResolvedType().(*types.FuncType); ok {
		calleeSig = ft
	}

	args, _, err := g.prepareCallArgs(calleeSig, e)
	if err != nil {
		return "", err
	}

	retType := e.GetResolvedType().(types.Type)
	retLLVM := g.llvmType(retType)

	if _, ok := retType.(*types.NoneType); ok {
		g.emitLine(fmt.Sprintf("  call void @%s(%s)", mangled, strings.Join(args, ", ")))
		return "void", nil
	}

	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call %s @%s(%s)", result, retLLVM, mangled, strings.Join(args, ", ")))
	return result, nil
}

// prepareCallArgs emits IR to evaluate every argument in the call (positional,
// *unpack, kwarg, **unpack), collects them into the right LLVM call shape for
// the callee, and returns the formatted "type val" parts to splice into the
// `call` instruction along with a parallel list of just-LLVM-type strings
// (used by emitVirtualCall to build the function-pointer cast). If callee is
// nil, falls back to plain positional pass-through.
func (g *Generator) prepareCallArgs(callee *types.FuncType, e *parser.CallExpr) ([]string, []string, error) {
	if callee == nil {
		parts := []string{}
		llvmTypes := []string{}
		for _, arg := range e.Args {
			val, err := g.emitExpr(arg)
			if err != nil {
				return nil, nil, err
			}
			at := arg.GetResolvedType().(types.Type)
			parts = append(parts, fmt.Sprintf("%s %s", g.llvmType(at), val))
			llvmTypes = append(llvmTypes, g.llvmType(at))
		}
		return parts, llvmTypes, nil
	}

	posVals := make([]callPosArg, 0, len(e.Args))
	for i, arg := range e.Args {
		v, err := g.emitExpr(arg)
		if err != nil {
			return nil, nil, err
		}
		t := arg.GetResolvedType().(types.Type)
		posVals = append(posVals, callPosArg{v, t, e.IsArgStar(i)})
	}

	kwVals := make([]callKwArg, 0, len(e.Kwargs))
	for _, kw := range e.Kwargs {
		v, err := g.emitExpr(kw.Value)
		if err != nil {
			return nil, nil, err
		}
		t := kw.Value.GetResolvedType().(types.Type)
		kwVals = append(kwVals, callKwArg{kw.Name, v, t, kw.IsDStar})
	}

	nNamed := len(callee.Params)
	kwOnly := callee.KwOnlyStart
	if callee.VarArgsElem == nil {
		kwOnly = nNamed
	}
	if kwOnly < 0 || kwOnly > nNamed {
		kwOnly = nNamed
	}

	namedVals := make([]string, nNamed)
	namedFilled := make([]bool, nNamed)

	overflow := []callPosArg{}

	posIdx := 0
	for _, pv := range posVals {
		if pv.isStar {
			overflow = append(overflow, pv)
			continue
		}
		if posIdx < kwOnly {
			namedVals[posIdx] = g.castToType(pv.val, pv.t, callee.Params[posIdx])
			namedFilled[posIdx] = true
			posIdx++
		} else {
			overflow = append(overflow, pv)
		}
	}

	leftoverKw := []callKwArg{}
	for _, kv := range kwVals {
		if kv.isDStar {
			leftoverKw = append(leftoverKw, kv)
			continue
		}
		matched := false
		for i := 0; i < nNamed; i++ {
			if !namedFilled[i] && i < len(callee.ParamNames) && callee.ParamNames[i] == kv.name {
				namedVals[i] = g.castToType(kv.val, kv.t, callee.Params[i])
				namedFilled[i] = true
				matched = true
				break
			}
		}
		if !matched {
			leftoverKw = append(leftoverKw, kv)
		}
	}

	parts := []string{}
	llvmTypes := []string{}

	emitNamedSlot := func(i int) error {
		if !namedFilled[i] {
			if i < len(callee.ParamDefaults) && callee.ParamDefaults[i] != nil {
				defExpr := callee.ParamDefaults[i]
				val, err := g.emitExpr(defExpr)
				if err != nil {
					return err
				}
				dt, _ := defExpr.GetResolvedType().(types.Type)
				if dt == nil {
					dt = callee.Params[i]
				}
				namedVals[i] = g.castToType(val, dt, callee.Params[i])
				namedFilled[i] = true
			} else {
				pname := fmt.Sprintf("#%d", i+1)
				if i < len(callee.ParamNames) {
					pname = callee.ParamNames[i]
				}
				return fmt.Errorf("internal: parameter %s not statically filled for call", pname)
			}
		}
		parts = append(parts, fmt.Sprintf("%s %s", g.llvmType(callee.Params[i]), namedVals[i]))
		llvmTypes = append(llvmTypes, g.llvmType(callee.Params[i]))
		return nil
	}

	// Emit in callee's source order: positional named, *args handle, kw-only
	// named, **kwargs handle. emitFuncDef walks fd.Params in this same order.
	for i := 0; i < kwOnly; i++ {
		if err := emitNamedSlot(i); err != nil {
			return nil, nil, err
		}
	}
	if callee.VarArgsElem != nil {
		listVal := g.buildVarArgsList(callee.VarArgsElem, overflow)
		parts = append(parts, "i8* "+listVal)
		llvmTypes = append(llvmTypes, "i8*")
	}
	for i := kwOnly; i < nNamed; i++ {
		if err := emitNamedSlot(i); err != nil {
			return nil, nil, err
		}
	}
	if callee.KwargsElem != nil {
		mapVal := g.buildKwargsMap(callee.KwargsElem, leftoverKw)
		parts = append(parts, "i8* "+mapVal)
		llvmTypes = append(llvmTypes, "i8*")
	}

	return parts, llvmTypes, nil
}

// callPosArg holds an evaluated positional argument (or *unpack source list)
// destined for a callee with *args.
type callPosArg struct {
	val    string
	t      types.Type
	isStar bool
}

// callKwArg holds an evaluated keyword argument (or **unpack source map)
// destined for a callee with **kwargs.
type callKwArg struct {
	name    string
	val     string
	t       types.Type
	isDStar bool
}

// buildVarArgsList emits IR that builds a fresh runtime list[elem] containing
// the overflow positional args (and *unpacked list contents in source order).
// Returns the LLVM value name of the resulting list handle (i8*).
func (g *Generator) buildVarArgsList(elem types.Type, overflow []callPosArg) string {
	elemSize := g.typeSize(elem)
	elemLLVM := g.llvmType(elem)

	listVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_list_new(i64 %d)", listVal, elemSize))

	for _, p := range overflow {
		if !p.isStar {
			v := g.castToType(p.val, p.t, elem)
			tmpAlloca := g.newTmp()
			g.emitAlloca(tmpAlloca, elemLLVM)
			g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, v, elemLLVM, tmpAlloca))
			cast := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", cast, elemLLVM, tmpAlloca))
			g.emitLine(fmt.Sprintf("  call void @spy_list_append(i8* %s, i8* %s)", listVal, cast))
			continue
		}
		// *unpack: iterate the source list and append each element.
		// Source list type is *types.ListType{Elem: srcElem}.
		srcList := p.val
		srcElem := elem
		if lt, ok := p.t.(*types.ListType); ok {
			srcElem = lt.Elem
		}
		srcElemLLVM := g.llvmType(srcElem)

		lenTmp := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_list_len(i8* %s)", lenTmp, srcList))

		idxAlloca := g.newTmp()
		g.emitAlloca(idxAlloca, "i64")
		g.emitLine(fmt.Sprintf("  store i64 0, i64* %s", idxAlloca))

		head := g.newLabel("vararg.unpack.head")
		body := g.newLabel("vararg.unpack.body")
		end := g.newLabel("vararg.unpack.end")

		g.emitLine(fmt.Sprintf("  br label %%%s", head))
		g.emitLine(fmt.Sprintf("%s:", head))
		idxV := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", idxV, idxAlloca))
		cmp := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = icmp slt i64 %s, %s", cmp, idxV, lenTmp))
		g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, body, end))

		g.emitLine(fmt.Sprintf("%s:", body))
		elemPtr := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_list_get(i8* %s, i64 %s)", elemPtr, srcList, idxV))
		// Load element from i8* as srcElemLLVM, then cast to elem if needed.
		typedPtr := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", typedPtr, elemPtr, srcElemLLVM))
		loaded := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", loaded, srcElemLLVM, srcElemLLVM, typedPtr))
		casted := g.castToType(loaded, srcElem, elem)

		// Append: alloca elem, store, bitcast, call append.
		tmpAlloca := g.newTmp()
		g.emitAlloca(tmpAlloca, elemLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, casted, elemLLVM, tmpAlloca))
		castVal := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", castVal, elemLLVM, tmpAlloca))
		g.emitLine(fmt.Sprintf("  call void @spy_list_append(i8* %s, i8* %s)", listVal, castVal))

		nextIdx := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = add i64 %s, 1", nextIdx, idxV))
		g.emitLine(fmt.Sprintf("  store i64 %s, i64* %s", nextIdx, idxAlloca))
		g.emitLine(fmt.Sprintf("  br label %%%s", head))

		g.emitLine(fmt.Sprintf("%s:", end))
	}

	return listVal
}

// buildKwargsMap emits IR that builds a fresh runtime map[str, value] from any
// leftover kwargs and **unpacks. Returns the LLVM value name of the map handle.
func (g *Generator) buildKwargsMap(value types.Type, leftover []callKwArg) string {
	valSize := g.typeSize(value)
	valLLVM := g.llvmType(value)

	mapVal := g.newTmp()
	// hashType=1 for str keys.
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_map_new(i64 8, i64 %d, i64 1)", mapVal, valSize))

	for _, kv := range leftover {
		if kv.isDStar {
			g.emitLine(fmt.Sprintf("  call void @spy_map_extend(i8* %s, i8* %s)", mapVal, kv.val))
			continue
		}
		// Static key: emit a string literal handle.
		keyVal := g.emitStringLiteral(kv.name)

		castedVal := g.castToType(kv.val, kv.t, value)

		keyAlloca := g.newTmp()
		g.emitAlloca(keyAlloca, "i8*")
		g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", keyVal, keyAlloca))
		keyCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast i8** %s to i8*", keyCast, keyAlloca))

		valAlloca := g.newTmp()
		g.emitAlloca(valAlloca, valLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", valLLVM, castedVal, valLLVM, valAlloca))
		valCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", valCast, valLLVM, valAlloca))

		g.emitLine(fmt.Sprintf("  call void @spy_map_set(i8* %s, i8* %s, i8* %s)", mapVal, keyCast, valCast))
	}

	return mapVal
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
	case *types.SetType:
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_set_len(i8* %s)", result, argVal))
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
	case *types.StrType:
		g.emitLine(fmt.Sprintf("  %s = call i64 @spy_str_to_int(i8* %s)", result, argVal))
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
	case *types.StrType:
		g.emitLine(fmt.Sprintf("  %s = call double @spy_str_to_float(i8* %s)", result, argVal))
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
		g.emitAlloca(keyAlloca, keyLLVM)
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

// emitSliceExpr lowers obj[low:high:step] to a runtime slice helper. Each
// missing bound is omitted from the call and signalled via a presence-flag
// bitmask: bit 0 = low, bit 1 = high, bit 2 = step. The runtime fills in
// Python's defaults (which depend on the sign of step) for any absent slot.
func (g *Generator) emitSliceExpr(e *parser.SliceExpr) (string, error) {
	objVal, err := g.emitExpr(e.Object)
	if err != nil {
		return "", err
	}

	flags := 0
	lowVal := "0"
	highVal := "0"
	stepVal := "0"
	if e.Low != nil {
		flags |= 1
		v, err := g.emitExpr(e.Low)
		if err != nil {
			return "", err
		}
		lowVal = v
	}
	if e.High != nil {
		flags |= 2
		v, err := g.emitExpr(e.High)
		if err != nil {
			return "", err
		}
		highVal = v
	}
	if e.Step != nil {
		flags |= 4
		v, err := g.emitExpr(e.Step)
		if err != nil {
			return "", err
		}
		stepVal = v
	}

	objType := e.Object.GetResolvedType().(types.Type)
	var fn string
	switch objType.(type) {
	case *types.StrType:
		fn = "spy_str_slice"
	case *types.BytesType:
		fn = "spy_bytes_slice"
	case *types.BytearrayType:
		fn = "spy_bytearray_slice"
	case *types.ListType:
		fn = "spy_list_slice"
	default:
		return "", fmt.Errorf("cannot slice %s", objType)
	}

	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @%s(i8* %s, i64 %s, i64 %s, i64 %s, i64 %d)",
		result, fn, objVal, lowVal, highVal, stepVal, flags))
	return result, nil
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
		g.emitAlloca(tmpAlloca, elemLLVM)
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
		// Upcast each value to the declared map value type. For map[K, Any]
		// this boxes int/float/bool/str/list/map/bytes into a SpyAny.
		if vt, ok := e.Values[i].GetResolvedType().(types.Type); ok {
			valVal = g.castToType(valVal, vt, mapType.Value)
		}

		keyAlloca := g.newTmp()
		g.emitAlloca(keyAlloca, keyLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", keyLLVM, keyVal, keyLLVM, keyAlloca))
		keyCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", keyCast, keyLLVM, keyAlloca))

		valAlloca := g.newTmp()
		g.emitAlloca(valAlloca, valLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", valLLVM, valVal, valLLVM, valAlloca))
		valCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", valCast, valLLVM, valAlloca))

		g.emitLine(fmt.Sprintf("  call void @spy_map_set(i8* %s, i8* %s, i8* %s)", mapVal, keyCast, valCast))
	}

	return mapVal, nil
}

func (g *Generator) emitSetLit(e *parser.SetLit) (string, error) {
	setType := e.GetResolvedType().(*types.SetType)
	keySize := g.typeSize(setType.Elem)

	hashType := 0
	if _, ok := setType.Elem.(*types.StrType); ok {
		hashType = 1
	}

	setVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_set_new(i64 %d, i64 %d)", setVal, keySize, hashType))

	elemLLVM := g.llvmType(setType.Elem)
	for _, el := range e.Elements {
		val, err := g.emitExpr(el)
		if err != nil {
			return "", err
		}
		if et, ok := el.GetResolvedType().(types.Type); ok {
			val = g.castToType(val, et, setType.Elem)
		}
		tmpAlloca := g.newTmp()
		g.emitAlloca(tmpAlloca, elemLLVM)
		g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, val, elemLLVM, tmpAlloca))
		tmpCast := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %s* %s to i8*", tmpCast, elemLLVM, tmpAlloca))
		g.emitLine(fmt.Sprintf("  call void @spy_set_add(i8* %s, i8* %s)", setVal, tmpCast))
	}

	return setVal, nil
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
	case *types.AnyType:
		return "i8*"
	case *types.ListType:
		return "i8*"
	case *types.MapType:
		return "i8*"
	case *types.SetType:
		return "i8*"
	case *types.FuncType:
		// First-class callable values are an i8* to a heap closure whose
		// first slot is the function pointer.
		return "i8*"
	case *types.InstanceType:
		return fmt.Sprintf("%%Class.%s*", v.Class.Name)
	case *types.IteratorType:
		// Iterators are opaque pointers — different generator types have
		// different concrete struct layouts but share a generic vtable
		// prefix that for-loops and next() dispatch through.
		_ = v
		return "i8*"
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
	case *types.BytesType:
		return 8
	case *types.BytearrayType:
		return 8
	case *types.AnyType:
		return 8 // pointer to SpyAny
	case *types.ListType:
		return 8
	case *types.MapType:
		return 8
	case *types.SetType:
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
	if g.funcBody != nil {
		g.funcBody.WriteString(line)
		g.funcBody.WriteString("\n")
		return
	}
	g.buf.WriteString(line)
	g.buf.WriteString("\n")
}

// emitAlloca emits `name = alloca llvmType` into the function's entry-block
// alloca buffer when one is active, otherwise inline. Allocas must live in
// the entry block: a dynamic alloca inside a loop is allocated on every
// iteration but only freed when the function returns, so a hot loop will
// blow the stack. Use this helper instead of writing the alloca inline.
func (g *Generator) emitAlloca(name, llvmType string) {
	line := fmt.Sprintf("  %s = alloca %s\n", name, llvmType)
	if g.funcAllocas != nil {
		g.funcAllocas.WriteString(line)
		return
	}
	g.buf.WriteString(line)
}

// emitEntry writes an instruction into the entry-block buffer (alongside the
// allocas), so it dominates every block in the function. Used for side-effect-
// free setup like jmp_buf bitcasts that must remain valid across the resume
// edges of a generator state machine, where the body block they'd otherwise
// live in can be bypassed by the dispatch switch.
func (g *Generator) emitEntry(line string) {
	if g.funcAllocas != nil {
		g.funcAllocas.WriteString(line)
		g.funcAllocas.WriteString("\n")
		return
	}
	g.buf.WriteString(line)
	g.buf.WriteString("\n")
}

// beginFunc swaps in fresh body/alloca buffers for the function currently
// being emitted. The caller is expected to have already written the
// `define ... {` and `entry:` lines into g.buf. Returns the previous
// buffer pointers so endFunc can restore them.
func (g *Generator) beginFunc() (*strings.Builder, *strings.Builder) {
	oldBody := g.funcBody
	oldAllocas := g.funcAllocas
	g.funcBody = &strings.Builder{}
	g.funcAllocas = &strings.Builder{}
	return oldBody, oldAllocas
}

// endFunc finishes the currently emitting function: writes the alloca
// buffer (entry-block allocas) followed by the body buffer to g.buf, and
// restores the previous buffer pointers.
func (g *Generator) endFunc(oldBody, oldAllocas *strings.Builder) {
	g.buf.WriteString(g.funcAllocas.String())
	g.buf.WriteString(g.funcBody.String())
	g.funcBody = oldBody
	g.funcAllocas = oldAllocas
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
			for _, p := range m.Params {
				if p.Default != nil {
					g.collectStringsInExpr(p.Default)
				}
			}
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
		for _, p := range s.Params {
			if p.Default != nil {
				g.collectStringsInExpr(p.Default)
			}
		}
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
		for _, kw := range e.Kwargs {
			if !kw.IsDStar {
				// Kwarg name is materialized as a runtime str at the call site.
				g.addStringConst(kw.Name)
			}
			g.collectStringsInExpr(kw.Value)
		}
	case *parser.IndexExpr:
		g.collectStringsInExpr(e.Object)
		g.collectStringsInExpr(e.Index)
	case *parser.SliceExpr:
		g.collectStringsInExpr(e.Object)
		if e.Low != nil {
			g.collectStringsInExpr(e.Low)
		}
		if e.High != nil {
			g.collectStringsInExpr(e.High)
		}
		if e.Step != nil {
			g.collectStringsInExpr(e.Step)
		}
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
	case *parser.SetLit:
		for _, el := range e.Elements {
			g.collectStringsInExpr(el)
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
