package codegen

import (
	"fmt"
	"strings"

	"github.com/yehoyadashtinmetz/spython/parser"
	"github.com/yehoyadashtinmetz/spython/types"
)

// Generators are compiled to a synthesized class implementing the iterator
// protocol. The class struct holds: a state index (case 0 = first call,
// 1..N = resume points after each yield, -1 = exhausted), all parameters
// (saved at construction), all locals discovered during a pre-walk of the
// body, one synthetic iterator slot per `yield from` site, and a `value`
// slot holding the most recently yielded value.
//
// The factory function (registered under the user's function name) builds
// the instance and returns it as an opaque `i8*` (which is what `Iterator[T]`
// resolves to). Iteration consumers (for-loops and the `next()` builtin)
// dispatch through a generic vtable shape — every generator's vtable starts
// with `{class_id, base_vtab, __iter___fn, __next___fn}` so we can call
// __next__ from a context that only knows the static type `Iterator[T]`.
//
// __next__ is the state machine: a switch on the state field jumps to either
// the body's entry block or one of the resume labels. Each `yield` emits
// `store value`, `store next-state`, `ret value`, `resume_<i>:`. End of body
// (or bare `return`) sets state = -1 and raises StopIteration.

type genLocal struct {
	name string
	typ  types.Type
}

type genLayout struct {
	stateIdx     int          // field index of the state slot
	paramStart   int          // first param field index
	localStart   int          // first user-local field index
	synthIterStart int        // first yield-from synthetic-iter field index
	valueIdx     int          // field index of the value (yielded) slot
	yieldType    types.Type   // T from Iterator[T]
	params       []parser.FuncParam
	paramTypes   []types.Type
	locals       []genLocal
	numYields    int          // total yield + yield-from sites (count of resume labels)
	numYieldFrom int          // count of yield-from sites
	numForIter   int          // count of `for x in <Iterator>` sites — each needs a struct slot for the iter handle so it survives across yields
}

// registerGenerator synthesizes a ClassType for the generator function and
// inserts it into the codegen tables so the regular class-emission passes
// (struct types, vtable globals) pick it up. The methods themselves are
// emitted later by emitGeneratorMethods.
func (g *Generator) registerGenerator(modID string, fd *parser.FuncDef) error {
	// The declared return type was validated by the checker as Iterator[T].
	retType := g.resolveTypeAnnotation(fd.ReturnType)
	it, ok := retType.(*types.IteratorType)
	if !ok {
		return fmt.Errorf("generator %s: return type did not resolve to Iterator[T]", fd.Name)
	}

	layout := &genLayout{yieldType: it.Elem}

	// Resolve parameter types from annotations. v1 generators don't accept
	// *args / **kwargs / defaults — keep it positional-only here.
	for _, p := range fd.Params {
		if p.Kind != parser.ParamPositional {
			return fmt.Errorf("generator %s: *args/**kwargs are not supported in v1", fd.Name)
		}
		if p.Default != nil {
			return fmt.Errorf("generator %s: parameters with default values are not supported in v1", fd.Name)
		}
		if p.TypeAnn == nil {
			return fmt.Errorf("generator %s: parameter %s requires a type annotation", fd.Name, p.Name)
		}
		pt := g.resolveTypeAnnotation(p.TypeAnn)
		if pt == nil {
			return fmt.Errorf("generator %s: unknown parameter type for %s", fd.Name, p.Name)
		}
		layout.params = append(layout.params, p)
		layout.paramTypes = append(layout.paramTypes, pt)
	}

	// Pre-walk the body to collect locals (assignments, multi-assigns,
	// for-loop vars, except-bound vars) and to count yields / yield-from
	// sites. Locals are discovered in source order; later occurrences of
	// the same name (re-assignment) don't create a new field.
	collectGenLocals(fd.Body, layout)

	// Build the field list: state + params + locals + synthetic iters + value.
	// Field 0 of the class struct (vtable pointer) is added by the existing
	// class layout machinery, so our indices below are 1-relative for the
	// `Fields` slice (i.e., Fields[i] becomes struct field i+1).
	fields := []types.ClassField{}
	fieldIdx := map[string]int{}
	addField := func(name string, t types.Type) int {
		idx := len(fields)
		fields = append(fields, types.ClassField{Name: name, Type: t})
		fieldIdx[name] = idx
		return idx
	}

	layout.stateIdx = addField("__state", &types.IntType{})
	layout.paramStart = len(fields)
	for i, p := range layout.params {
		addField(p.Name, layout.paramTypes[i])
	}
	layout.localStart = len(fields)
	for _, l := range layout.locals {
		// Skip locals whose name shadows a parameter; the parameter slot
		// already covers them (Python semantics: a parameter is an
		// in-scope local for assignment).
		if _, exists := fieldIdx[l.name]; exists {
			continue
		}
		addField(l.name, l.typ)
	}
	layout.synthIterStart = len(fields)
	for i := 0; i < layout.numYieldFrom; i++ {
		addField(fmt.Sprintf("__yfiter%d", i), &types.IteratorType{Elem: layout.yieldType})
	}
	for i := 0; i < layout.numForIter; i++ {
		addField(fmt.Sprintf("__foriter%d", i), &types.IteratorType{Elem: layout.yieldType})
	}
	layout.valueIdx = addField("__value", layout.yieldType)

	// Build the synthetic ClassType.
	className := fmt.Sprintf("__gen_%s_%s", modID, fd.Name)
	classID := g.nextClassID()
	ct := &types.ClassType{
		Name:       className,
		Fields:     fields,
		FieldIdx:   fieldIdx,
		Methods:    map[string]*types.FuncType{},
		OwnMethods: map[string]bool{"__iter__": true, "__next__": true},
		MethodSrc:  map[string]*types.ClassType{},
		ClassID:    classID,
		DefinedIn:  modID,
	}
	// __next__'s signature on the class — used by methodFuncType when
	// emitting the vtable so the function pointer is cast to T (i8*).
	ct.Methods["__next__"] = &types.FuncType{Params: nil, Return: layout.yieldType, DefinedIn: modID}
	// __iter__ keeps the synthetic signature `i8* (Class*)*` (returns self).

	// Register so emitClassTypes / emitVTable see it. classDef stays nil
	// because there's no parser ClassDef — emitClassMethods short-circuits
	// for genClassSet entries.
	g.classes = append(g.classes, ct)
	g.classByName[className] = ct
	g.classModule[ct] = modID
	g.genClassSet[ct] = true
	g.genFuncClass[fd] = ct
	g.genClassFunc[ct] = fd
	g.genLayouts[ct] = layout

	// Manually populate the vtable slot tables (computeSlots is called for
	// regular classes via emitClassTypes; we bypass it). __iter__ is slot
	// 0 and __next__ slot 1 by convention — for-loops dispatch on those
	// fixed indices on opaque iterators.
	g.methodSlots[ct] = map[string]int{"__iter__": 0, "__next__": 1}
	g.slotOrder[ct] = []string{"__iter__", "__next__"}
	g.slotOwner[ct] = []*types.ClassType{ct, ct}

	return nil
}

// nextClassID returns a unique class id. Reuses the same source the loader
// installed via SetClassIDSource for regular classes when that's available;
// otherwise falls back to a strictly-monotonic local counter so tests that
// drive the codegen directly still get unique ids.
func (g *Generator) nextClassID() int {
	// Find the maximum existing class id and add one. This is O(N) but only
	// runs once per generator at codegen-init time.
	max := 0
	for _, ct := range g.classes {
		if ct.ClassID > max {
			max = ct.ClassID
		}
	}
	return max + 1
}

// collectGenLocals walks the body to populate layout.locals (each unique
// local name with its type) and to count yield and yield-from sites.
func collectGenLocals(body []parser.Stmt, layout *genLayout) {
	seen := map[string]bool{}
	// Parameters take precedence: their fields already cover the names, so
	// don't re-add them as locals.
	for _, p := range layout.params {
		seen[p.Name] = true
	}
	addLocal := func(name string, t types.Type) {
		if seen[name] || t == nil {
			return
		}
		seen[name] = true
		layout.locals = append(layout.locals, genLocal{name: name, typ: t})
	}

	var walk func([]parser.Stmt)
	walk = func(stmts []parser.Stmt) {
		for _, s := range stmts {
			switch x := s.(type) {
			case *parser.YieldStmt:
				layout.numYields++
				if x.IsFrom {
					layout.numYieldFrom++
				}
			case *parser.AssignStmt:
				addLocal(x.Name, resolvedTypeOf(x.Value, x.TypeAnn))
			case *parser.AugAssignStmt:
				// Augmented assignment never introduces a new local.
			case *parser.MultiAssignStmt:
				if tup, ok := x.Value.GetResolvedType().(*types.TupleType); ok {
					for i, name := range x.Names {
						if i < len(tup.Elements) {
							addLocal(name, tup.Elements[i])
						}
					}
				}
			case *parser.ForStmt:
				if it, ok := x.Iter.GetResolvedType().(types.Type); ok {
					var elem types.Type
					switch t := it.(type) {
					case *types.ListType:
						elem = t.Elem
					case *types.IteratorType:
						elem = t.Elem
						// The iterator handle must survive across yields,
						// so reserve a struct slot for it.
						layout.numForIter++
					case *types.IntType:
						elem = &types.IntType{}
					case *types.StrType:
						elem = &types.StrType{}
					}
					addLocal(x.VarName, elem)
				}
				walk(x.Body)
			case *parser.IfStmt:
				walk(x.Body)
				for _, e := range x.Elifs {
					walk(e.Body)
				}
				walk(x.ElseBody)
			case *parser.WhileStmt:
				walk(x.Body)
			case *parser.TryStmt:
				walk(x.Body)
				for _, ec := range x.Excepts {
					if ec.VarName != "" && ec.ExcType != nil {
						// We resolve the except var type lazily during
						// emission since the checker doesn't store the
						// resolved annotation back; for now skip it from
						// the field list — except-bound vars in v1
						// generators are out of scope.
					}
					walk(ec.Body)
				}
				walk(x.FinallyBody)
			}
		}
	}
	walk(body)
}

func resolvedTypeOf(e parser.Expr, ann *parser.TypeAnnotation) types.Type {
	if t, ok := e.GetResolvedType().(types.Type); ok && t != nil {
		return t
	}
	_ = ann // future: resolve from annotation when checker doesn't set on Value
	return nil
}

// emitGeneratorFactory emits the LLVM function that constructs a fresh
// generator instance. It replaces the regular emitFuncDef path for
// generator funcdefs and is registered under the user-visible function
// name (e.g. @spy_main_count for `def count(...)`). The returned value is
// `i8*` — the IteratorType pointer.
func (g *Generator) emitGeneratorFactory(fd *parser.FuncDef) error {
	ct, ok := g.genFuncClass[fd]
	if !ok {
		return fmt.Errorf("emitGeneratorFactory: no gen class for %s", fd.Name)
	}
	layout := g.genLayouts[ct]

	params := []string{}
	for i, p := range layout.params {
		params = append(params, fmt.Sprintf("%s %%%s", g.llvmType(layout.paramTypes[i]), p.Name))
	}
	g.emitLine(fmt.Sprintf("define i8* @spy_%s_%s(%s) {", g.currentMod, fd.Name, strings.Join(params, ", ")))
	g.emitLine("entry:")
	oldBody, oldAllocas := g.beginFunc()

	// Allocate the instance via spy_instance_new. Match emitConstructorCall:
	// 8 bytes for the vtable pointer plus 8 per field (over-allocating
	// alignment-safely). Boehm GC zero-initializes — locals start at 0/null.
	size := int64(8)
	for _, f := range ct.Fields {
		size += int64(g.fieldAllocSize(f.Type))
	}
	rawPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_instance_new(i64 %d)", rawPtr, size))
	instPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %%Class.%s*", instPtr, rawPtr, ct.Name))

	// Vtable pointer (struct slot 0).
	vtabSlotPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %%Class.%s* %s, i32 0, i32 0",
		vtabSlotPtr, ct.Name, ct.Name, instPtr))
	vtabGeneric := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %%VTable.%s* @vtable.%s to i8*",
		vtabGeneric, ct.Name, ct.Name))
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", vtabGeneric, vtabSlotPtr))

	// State = 0 (initial entry).
	stateFieldPtr := g.fieldGEP(instPtr, ct, layout.stateIdx)
	g.emitLine(fmt.Sprintf("  store i64 0, i64* %s", stateFieldPtr))

	// Save each parameter into its field.
	for i, p := range layout.params {
		fieldPtr := g.fieldGEP(instPtr, ct, layout.paramStart+i)
		llvmT := g.llvmType(layout.paramTypes[i])
		g.emitLine(fmt.Sprintf("  store %s %%%s, %s* %s", llvmT, p.Name, llvmT, fieldPtr))
	}

	// Return as opaque i8* (matches IteratorType's LLVM type).
	g.emitLine(fmt.Sprintf("  ret i8* %s", rawPtr))
	g.endFunc(oldBody, oldAllocas)
	g.emitLine("}")
	return nil
}

// fieldGEP emits a getelementptr to field index `idx` (0-relative within
// ct.Fields, i.e., the LLVM struct field index is idx+1 because slot 0 is
// the vtable pointer). Returns the SSA name of the typed pointer.
func (g *Generator) fieldGEP(instPtr string, ct *types.ClassType, idx int) string {
	tmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %%Class.%s* %s, i32 0, i32 %d",
		tmp, ct.Name, ct.Name, instPtr, idx+1))
	return tmp
}

// emitGeneratorMethods emits __iter__ and __next__ for a generator's
// synthetic class. Called once per generator funcdef, after the regular
// class methods for the same module are emitted.
func (g *Generator) emitGeneratorMethods(fd *parser.FuncDef) error {
	ct := g.genFuncClass[fd]
	if ct == nil {
		return fmt.Errorf("emitGeneratorMethods: no gen class for %s", fd.Name)
	}
	g.emitGeneratorIter(ct)
	g.emitLine("")
	if err := g.emitGeneratorNext(ct, fd); err != nil {
		return err
	}
	g.emitLine("")
	return nil
}

// emitGeneratorIter emits a trivial __iter__ that returns self.
func (g *Generator) emitGeneratorIter(ct *types.ClassType) {
	mangled := g.methodMangled(ct, "__iter__")
	selfLLVM := fmt.Sprintf("%%Class.%s*", ct.Name)
	g.emitLine(fmt.Sprintf("define i8* %s(%s %%self) {", mangled, selfLLVM))
	g.emitLine("entry:")
	cast := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %s %%self to i8*", cast, selfLLVM))
	g.emitLine(fmt.Sprintf("  ret i8* %s", cast))
	g.emitLine("}")
}

// emitGeneratorNext emits the state-machine __next__. Layout:
//
//	entry:
//	  ; precompute GEPs for state, locals, value
//	  ; load state and switch into the right block
//	  switch i64 %state, label %exhausted [
//	    i64 0, label %case_0
//	    i64 1, label %resume_1
//	    ...
//	  ]
//	case_0:
//	  ; user body, with each yield emitting ret + resume_<i>:
//	  br label %exhausted
//	exhausted:
//	  ; mark state = -1 and raise StopIteration
func (g *Generator) emitGeneratorNext(ct *types.ClassType, fd *parser.FuncDef) error {
	layout := g.genLayouts[ct]
	mangled := g.methodMangled(ct, "__next__")
	selfLLVM := fmt.Sprintf("%%Class.%s*", ct.Name)
	retLLVM := g.llvmType(layout.yieldType)

	g.emitLine(fmt.Sprintf("define %s %s(%s %%self) {", retLLVM, mangled, selfLLVM))
	g.emitLine("entry:")
	oldBody, oldAllocas := g.beginFunc()

	// Save and restore checker-shaped state on the Generator.
	oldVars := g.vars
	oldInFunc := g.inFunction
	oldRet := g.currentReturnType
	oldRetLLVM := g.currentReturnLLVMType
	oldGenCtx := g.genCtx
	g.vars = map[string]varInfo{}
	g.inFunction = true
	g.currentReturnType = layout.yieldType
	g.currentReturnLLVMType = retLLVM

	// Stash a context object so YieldStmt emission can reach the layout/
	// state-machine helpers.
	g.genCtx = &genEmitCtx{
		ct:           ct,
		layout:       layout,
		exhaustedLbl: g.newLabel("gen.exhausted"),
		yieldCount:   0,
		yfCount:      0,
	}

	// Precompute GEPs for state, value, params, locals, and synthetic-iter
	// fields. All locals are addressed via these self-field pointers, so
	// existing emit code (load/store via varInfo.llvmName) just works.
	statePtr := g.fieldGEP("%self", ct, layout.stateIdx)
	g.genCtx.statePtr = statePtr
	g.genCtx.valuePtr = g.fieldGEP("%self", ct, layout.valueIdx)

	for i, p := range layout.params {
		fieldPtr := g.fieldGEP("%self", ct, layout.paramStart+i)
		g.vars[p.Name] = varInfo{llvmName: fieldPtr, typ: layout.paramTypes[i]}
	}
	for i, l := range layout.locals {
		fieldPtr := g.fieldGEP("%self", ct, layout.localStart+i)
		g.vars[l.name] = varInfo{llvmName: fieldPtr, typ: l.typ}
	}
	for i := 0; i < layout.numYieldFrom; i++ {
		fieldPtr := g.fieldGEP("%self", ct, layout.synthIterStart+i)
		g.genCtx.yfPtrs = append(g.genCtx.yfPtrs, fieldPtr)
	}
	for i := 0; i < layout.numForIter; i++ {
		fieldPtr := g.fieldGEP("%self", ct, layout.synthIterStart+layout.numYieldFrom+i)
		g.genCtx.foriterPtrs = append(g.genCtx.foriterPtrs, fieldPtr)
	}

	// Dispatch on state. case 0 is the initial entry; cases 1..N are
	// resume points. State < 0 (we set -1 on exhaustion) falls through
	// the switch to `exhausted`.
	stateVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i64, i64* %s", stateVal, statePtr))
	caseLabels := make([]string, layout.numYields+1)
	caseLabels[0] = g.newLabel("gen.case0")
	for i := 1; i <= layout.numYields; i++ {
		caseLabels[i] = g.newLabel(fmt.Sprintf("gen.resume.%d", i))
	}
	g.genCtx.caseLabels = caseLabels

	caseStrs := make([]string, 0, len(caseLabels))
	for i, lbl := range caseLabels {
		caseStrs = append(caseStrs, fmt.Sprintf("i64 %d, label %%%s", i, lbl))
	}
	g.emitLine(fmt.Sprintf("  switch i64 %s, label %%%s [ %s ]",
		stateVal, g.genCtx.exhaustedLbl, strings.Join(caseStrs, " ")))

	// Emit the body starting at case 0.
	g.emitLine(fmt.Sprintf("%s:", caseLabels[0]))
	for _, st := range fd.Body {
		if err := g.emitStmt(st); err != nil {
			return err
		}
	}
	// Falling off the end of the body is equivalent to exhausting.
	g.emitLine(fmt.Sprintf("  br label %%%s", g.genCtx.exhaustedLbl))

	// Exhaustion path: mark state = -1, raise StopIteration.
	g.emitLine(fmt.Sprintf("%s:", g.genCtx.exhaustedLbl))
	g.emitLine(fmt.Sprintf("  store i64 -1, i64* %s", statePtr))
	g.emitRaiseStopIteration()
	g.emitLine("  unreachable")

	g.endFunc(oldBody, oldAllocas)
	g.emitLine("}")

	g.vars = oldVars
	g.inFunction = oldInFunc
	g.currentReturnType = oldRet
	g.currentReturnLLVMType = oldRetLLVM
	g.genCtx = oldGenCtx
	return nil
}

// genEmitCtx carries per-generator state-machine info that emitYieldStmt
// needs to reach. Stored on g during emitGeneratorNext; nil otherwise.
type genEmitCtx struct {
	ct           *types.ClassType
	layout       *genLayout
	statePtr     string   // SSA name of the state field GEP (i64*)
	valuePtr     string   // SSA name of the value field GEP (T*)
	yfPtrs       []string // SSA names for synthetic yield-from iter fields (i8**)
	foriterPtrs  []string // SSA names for synthetic for-iter handle fields (i8**)
	caseLabels   []string // [case0, resume_1, ..., resume_N]
	exhaustedLbl string
	yieldCount   int // bumped at each yield, indexes into caseLabels[1..]
	yfCount      int // bumped at each yield-from, indexes into yfPtrs
	foriterCount int // bumped at each for-iter, indexes into foriterPtrs
}

// emitYieldStmt lowers a single yield (or yield-from) statement. Called
// from emitStmt when it sees a *parser.YieldStmt and the current function
// is a generator.
func (g *Generator) emitYieldStmt(s *parser.YieldStmt) error {
	if g.genCtx == nil {
		return fmt.Errorf("yield outside generator codegen context")
	}
	if s.IsFrom {
		return g.emitYieldFromStmt(s)
	}
	return g.emitYieldValueStmt(s)
}

// emitYieldValueStmt: yield <expr> — store value, set state to next-resume,
// return value, then emit the resume label so subsequent statements
// continue in the next basic block.
func (g *Generator) emitYieldValueStmt(s *parser.YieldStmt) error {
	val, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}
	yieldT := g.genCtx.layout.yieldType
	llvmT := g.llvmType(yieldT)
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", llvmT, val, llvmT, g.genCtx.valuePtr))

	g.genCtx.yieldCount++
	resumeIdx := g.genCtx.yieldCount
	g.emitLine(fmt.Sprintf("  store i64 %d, i64* %s", resumeIdx, g.genCtx.statePtr))
	g.emitLine(fmt.Sprintf("  ret %s %s", llvmT, val))
	g.emitLine(fmt.Sprintf("%s:", g.genCtx.caseLabels[resumeIdx]))
	return nil
}

// emitYieldFromStmt: yield from <inner_iter> — repeatedly pull a value from
// the inner iterator and re-yield it until StopIteration. The inner
// iterator is stashed in a synthetic field so it survives across the
// per-element yield (which exits __next__ and re-enters later).
func (g *Generator) emitYieldFromStmt(s *parser.YieldStmt) error {
	innerVal, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}
	yfIdx := g.genCtx.yfCount
	g.genCtx.yfCount++
	yfPtr := g.genCtx.yfPtrs[yfIdx]
	// Store the inner iterator into the synthetic field. Also call
	// __iter__ to satisfy the protocol — for generators this is the
	// identity but we don't bake that assumption in.
	iterVal := g.emitGenVtableCall(innerVal, 2, "i8*", nil)
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", iterVal, yfPtr))

	yieldT := g.genCtx.layout.yieldType
	llvmT := g.llvmType(yieldT)

	loopHead := g.newLabel("yf.head")
	tryLbl := g.newLabel("yf.try")
	dispatchLbl := g.newLabel("yf.dispatch")
	stopLbl := g.newLabel("yf.stop")
	rethrowLbl := g.newLabel("yf.rethrow")
	endLbl := g.newLabel("yf.end")

	bufArr := g.newTmp()
	g.emitAlloca(bufArr, "[256 x i8]")
	bufI8 := g.newTmp()
	// Hoist the jmp_buf bitcast into the entry block: the loop head is
	// re-entered through the generator dispatch switch (after a yield),
	// which bypasses this setup, so an inline bitcast would not dominate.
	g.emitEntry(fmt.Sprintf("  %s = bitcast [256 x i8]* %s to i8*", bufI8, bufArr))

	g.emitLine(fmt.Sprintf("  br label %%%s", loopHead))
	g.emitLine(fmt.Sprintf("%s:", loopHead))
	curIter := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", curIter, yfPtr))
	g.emitLine(fmt.Sprintf("  call void @spy_exc_push(i8* %s)", bufI8))
	sj := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i32 @setjmp(i8* %s)", sj, bufI8))
	cmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp eq i32 %s, 0", cmp, sj))
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, tryLbl, dispatchLbl))

	g.emitLine(fmt.Sprintf("%s:", tryLbl))
	val := g.emitGenVtableCall(curIter, 3, llvmT, nil)
	g.emitLine("  call void @spy_exc_pop()")
	// Yield the pulled value.
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", llvmT, val, llvmT, g.genCtx.valuePtr))
	g.genCtx.yieldCount++
	resumeIdx := g.genCtx.yieldCount
	g.emitLine(fmt.Sprintf("  store i64 %d, i64* %s", resumeIdx, g.genCtx.statePtr))
	g.emitLine(fmt.Sprintf("  ret %s %s", llvmT, val))
	g.emitLine(fmt.Sprintf("%s:", g.genCtx.caseLabels[resumeIdx]))
	g.emitLine(fmt.Sprintf("  br label %%%s", loopHead))

	g.emitLine(fmt.Sprintf("%s:", dispatchLbl))
	g.emitLine("  call void @spy_exc_pop()")
	excRaw := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_exc_current()", excRaw))
	si, ok := g.classByName["StopIteration"]
	if !ok {
		return fmt.Errorf("StopIteration class not registered")
	}
	cond := g.emitIsInstanceRaw(excRaw, si.ClassID)
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cond, stopLbl, rethrowLbl))

	g.emitLine(fmt.Sprintf("%s:", stopLbl))
	g.emitLine("  call void @spy_exc_clear()")
	g.emitLine(fmt.Sprintf("  br label %%%s", endLbl))

	g.emitLine(fmt.Sprintf("%s:", rethrowLbl))
	g.emitLine("  call void @spy_exc_rethrow()")
	g.emitLine("  unreachable")

	g.emitLine(fmt.Sprintf("%s:", endLbl))
	return nil
}

// emitRaiseStopIteration constructs and throws a StopIteration instance.
// Mirrors emitDivZeroCheck's exception construction.
func (g *Generator) emitRaiseStopIteration() {
	si, ok := g.classByName["StopIteration"]
	if !ok {
		g.emitLine("  ; StopIteration class missing — cannot raise")
		return
	}
	msg := "StopIteration"
	g.addStringConst(msg)
	idx := g.getStringIndex(msg)
	msgSSA := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_new(i8* getelementptr ([%d x i8], [%d x i8]* @.str.%d, i64 0, i64 0), i64 %d)",
		msgSSA, len(msg), len(msg), idx, len(msg)))
	inst, err := g.emitSyntheticConstructor(si, []syntheticArg{{llvmType: "i8*", val: msgSSA}})
	if err != nil {
		g.emitLine("  ; failed to construct StopIteration")
		return
	}
	rawInst := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %%Class.%s* %s to i8*", rawInst, si.Name, inst))
	g.emitLine(fmt.Sprintf("  call void @spy_exc_throw(i8* %s)", rawInst))
}

// emitGenVtableCall dispatches a method on an opaque iterator value. The
// generic vtable layout for any generator class is:
//
//	{i32 class_id, i8* base_vtab, i8* __iter__, i8* __next__}
//
// vtSlotIdx is the field index in that struct (2 for __iter__, 3 for
// __next__). retLLVM is the LLVM return type the call site expects;
// callers monomorphize at the call site since IteratorType.Elem is
// statically known.
func (g *Generator) emitGenVtableCall(iterVal string, vtSlotIdx int, retLLVM string, extraArgs []string) string {
	// Read vtable pointer at offset 0 of the instance.
	hdr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to {i8*}*", hdr, iterVal))
	vtSlotPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr {i8*}, {i8*}* %s, i32 0, i32 0", vtSlotPtr, hdr))
	vt := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", vt, vtSlotPtr))

	// Cast vtable to the generic gen-vtable shape.
	vtTyped := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to {i32, i8*, i8*, i8*}*", vtTyped, vt))
	fnSlotPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr {i32, i8*, i8*, i8*}, {i32, i8*, i8*, i8*}* %s, i32 0, i32 %d",
		fnSlotPtr, vtTyped, vtSlotIdx))
	fnOpaque := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", fnOpaque, fnSlotPtr))

	// Build the function-pointer type and bitcast.
	argTypes := []string{"i8*"}
	argTypes = append(argTypes, extraArgs...)
	fnType := fmt.Sprintf("%s (%s)", retLLVM, strings.Join(argTypes, ", "))
	fnTyped := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", fnTyped, fnOpaque, fnType))

	allArgs := []string{fmt.Sprintf("i8* %s", iterVal)}
	allArgs = append(allArgs, extraArgs...)
	if retLLVM == "void" {
		g.emitLine(fmt.Sprintf("  call void %s(%s)", fnTyped, strings.Join(allArgs, ", ")))
		return ""
	}
	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call %s %s(%s)", result, retLLVM, fnTyped, strings.Join(allArgs, ", ")))
	return result
}

// emitNextCall lowers the `next(g)` builtin: dispatch __next__ on the
// opaque iterator. The returned LLVM value type is monomorphized from
// the call site's resolved IteratorType[T].
func (g *Generator) emitNextCall(e *parser.CallExpr) (string, error) {
	if len(e.Args) != 1 {
		return "", fmt.Errorf("next() takes exactly 1 argument")
	}
	iterVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}
	it, ok := e.Args[0].GetResolvedType().(*types.IteratorType)
	if !ok {
		return "", fmt.Errorf("next() argument did not resolve to Iterator[T]")
	}
	return g.emitGenVtableCall(iterVal, 3, g.llvmType(it.Elem), nil), nil
}

// emitForIterator handles `for x in iter_expr:` where iter_expr's static
// type is IteratorType[T]. Calls __iter__ once, then loops calling
// __next__ until it raises StopIteration.
func (g *Generator) emitForIterator(s *parser.ForStmt, iterVal string, it *types.IteratorType) error {
	elemLLVM := g.llvmType(it.Elem)

	// Normalize via __iter__ (for generators this returns self; we honor
	// the protocol so user-iterator classes can plug in later).
	iter := g.emitGenVtableCall(iterVal, 2, "i8*", nil)

	// Iter handle storage: in a generator, must live in a struct slot so it
	// survives across yields; otherwise a stack alloca is fine.
	var iterAlloca string
	if g.genCtx != nil && g.genCtx.foriterCount < len(g.genCtx.foriterPtrs) {
		iterAlloca = g.genCtx.foriterPtrs[g.genCtx.foriterCount]
		g.genCtx.foriterCount++
	} else {
		iterAlloca = g.newTmp()
		g.emitAlloca(iterAlloca, "i8*")
	}
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", iter, iterAlloca))

	// Loop-var storage: in a generator, the loop var is already a struct
	// field (collectGenLocals registered it), so reuse that binding instead
	// of clobbering with a fresh stack alloca that won't survive yields.
	var loopVar string
	if g.genCtx != nil {
		if info, ok := g.vars[s.VarName]; ok {
			loopVar = info.llvmName
		}
	}
	if loopVar == "" {
		loopVar = g.newTmp()
		g.emitAlloca(loopVar, elemLLVM)
		g.vars[s.VarName] = varInfo{llvmName: loopVar, typ: it.Elem}
	}

	bufArr := g.newTmp()
	g.emitAlloca(bufArr, "[256 x i8]")
	bufI8 := g.newTmp()
	// Hoist the jmp_buf bitcast into the entry block so it dominates the loop
	// head, which is re-entered through the generator dispatch switch.
	g.emitEntry(fmt.Sprintf("  %s = bitcast [256 x i8]* %s to i8*", bufI8, bufArr))

	loopHead := g.newLabel("foriter.head")
	tryLbl := g.newLabel("foriter.try")
	bodyLbl := g.newLabel("foriter.body")
	dispatchLbl := g.newLabel("foriter.dispatch")
	stopLbl := g.newLabel("foriter.stop")
	rethrowLbl := g.newLabel("foriter.rethrow")
	endLbl := g.newLabel("foriter.end")

	g.breakLabels = append(g.breakLabels, endLbl)
	g.continueLabels = append(g.continueLabels, loopHead)

	g.emitLine(fmt.Sprintf("  br label %%%s", loopHead))
	g.emitLine(fmt.Sprintf("%s:", loopHead))
	g.emitLine(fmt.Sprintf("  call void @spy_exc_push(i8* %s)", bufI8))
	sj := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i32 @setjmp(i8* %s)", sj, bufI8))
	cmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp eq i32 %s, 0", cmp, sj))
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, tryLbl, dispatchLbl))

	g.emitLine(fmt.Sprintf("%s:", tryLbl))
	curIter := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", curIter, iterAlloca))
	val := g.emitGenVtableCall(curIter, 3, elemLLVM, nil)
	g.emitLine("  call void @spy_exc_pop()")
	g.emitLine(fmt.Sprintf("  store %s %s, %s* %s", elemLLVM, val, elemLLVM, loopVar))
	g.emitLine(fmt.Sprintf("  br label %%%s", bodyLbl))

	g.emitLine(fmt.Sprintf("%s:", bodyLbl))
	for _, st := range s.Body {
		if err := g.emitStmt(st); err != nil {
			return err
		}
	}
	g.emitLine(fmt.Sprintf("  br label %%%s", loopHead))

	g.emitLine(fmt.Sprintf("%s:", dispatchLbl))
	g.emitLine("  call void @spy_exc_pop()")
	excRaw := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_exc_current()", excRaw))
	si, ok := g.classByName["StopIteration"]
	if !ok {
		return fmt.Errorf("StopIteration class not registered")
	}
	stopCond := g.emitIsInstanceRaw(excRaw, si.ClassID)
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", stopCond, stopLbl, rethrowLbl))

	g.emitLine(fmt.Sprintf("%s:", stopLbl))
	g.emitLine("  call void @spy_exc_clear()")
	g.emitLine(fmt.Sprintf("  br label %%%s", endLbl))

	g.emitLine(fmt.Sprintf("%s:", rethrowLbl))
	g.emitLine("  call void @spy_exc_rethrow()")
	g.emitLine("  unreachable")

	g.emitLine(fmt.Sprintf("%s:", endLbl))
	g.breakLabels = g.breakLabels[:len(g.breakLabels)-1]
	g.continueLabels = g.continueLabels[:len(g.continueLabels)-1]
	return nil
}
