package codegen

import (
	"fmt"
	"strings"

	"github.com/yehoyadashtinmetz/spython/parser"
	"github.com/yehoyadashtinmetz/spython/types"
)

// emitClassTypes declares LLVM struct types for each class and computes
// method-vtable slot assignments. __init__ is excluded from the vtable; all
// other methods (including auto-defaulted __str__/__repr__) occupy slots.
func (g *Generator) emitClassTypes() {
	// Concrete struct definitions. LLVM permits forward references to not-yet-
	// declared struct types via their pointer form, so ordering doesn't matter.
	for _, ct := range g.classes {
		parts := []string{"i8*"} // slot 0: vtable pointer
		for _, f := range ct.Fields {
			parts = append(parts, g.llvmType(f.Type))
		}
		g.emitLine(fmt.Sprintf("%%Class.%s = type { %s }", ct.Name, strings.Join(parts, ", ")))
	}
	g.emitLine("")

	// Compute method slots per class.
	for _, ct := range g.classes {
		g.computeSlots(ct)
	}

	// VTable struct definitions.
	for _, ct := range g.classes {
		parts := []string{"i32", "i8*"} // class_id, base_vtable_ptr
		for range g.slotOrder[ct] {
			parts = append(parts, "i8*")
		}
		g.emitLine(fmt.Sprintf("%%VTable.%s = type { %s }", ct.Name, strings.Join(parts, ", ")))
	}
	g.emitLine("")
}

// computeSlots populates g.methodSlots/g.slotOrder/g.slotOwner for a class,
// walking from root to leaf. Slots inherited from the base come first; new
// methods defined on this class are appended at the end. Overriding a base
// method reuses the base's slot but changes the slot owner.
func (g *Generator) computeSlots(ct *types.ClassType) {
	var baseOrder []string
	var baseOwners []*types.ClassType
	if ct.Base != nil {
		baseOrder = g.slotOrder[ct.Base]
		baseOwners = g.slotOwner[ct.Base]
	}

	slotMap := map[string]int{}
	order := make([]string, len(baseOrder))
	owners := make([]*types.ClassType, len(baseOwners))
	copy(order, baseOrder)
	copy(owners, baseOwners)
	for i, name := range order {
		slotMap[name] = i
	}

	cd := g.classDef[ct]
	if cd != nil {
		for _, m := range cd.Methods {
			if m.Name == "__init__" {
				continue
			}
			if existing, ok := slotMap[m.Name]; ok {
				// Overriding: change owner at existing slot.
				owners[existing] = ct
				continue
			}
			slotMap[m.Name] = len(order)
			order = append(order, m.Name)
			owners = append(owners, ct)
		}
	}

	// Also include auto-synthesized __str__/__repr__ if absent.
	if _, ok := slotMap["__str__"]; !ok {
		slotMap["__str__"] = len(order)
		order = append(order, "__str__")
		owners = append(owners, ct)
	}
	if _, ok := slotMap["__repr__"]; !ok {
		slotMap["__repr__"] = len(order)
		order = append(order, "__repr__")
		owners = append(owners, ct)
	}

	g.methodSlots[ct] = slotMap
	g.slotOrder[ct] = order
	g.slotOwner[ct] = owners
}

// methodMangled returns the LLVM global name for a class method.
func (g *Generator) methodMangled(ct *types.ClassType, methodName string) string {
	mod := g.classModule[ct]
	return fmt.Sprintf("@spy_%s_%s_%s", mod, ct.Name, methodName)
}

// emitClassMethods emits LLVM function definitions for all user-defined
// methods on a class plus auto-synthesized __str__/__repr__ as needed.
func (g *Generator) emitClassMethods(ct *types.ClassType) error {
	cd := g.classDef[ct]
	definedMethods := map[string]bool{}
	if cd != nil {
		for _, m := range cd.Methods {
			definedMethods[m.Name] = true
			if err := g.emitMethodFuncDef(ct, m); err != nil {
				return err
			}
			g.emitLine("")
		}
	}

	if !definedMethods["__str__"] {
		g.emitSyntheticStr(ct, "__str__")
		g.emitLine("")
	}
	if !definedMethods["__repr__"] {
		g.emitSyntheticStr(ct, "__repr__")
		g.emitLine("")
	}

	return nil
}

// emitMethodFuncDef emits a single method as a mangled LLVM function.
// The method body may reference `self`, fields via `self.x`, sibling methods
// via `self.method()`, and `super().method()`.
func (g *Generator) emitMethodFuncDef(ct *types.ClassType, m *parser.FuncDef) error {
	mangled := g.methodMangled(ct, m.Name)
	// Method signature: first param is self (class instance pointer).
	retType := types.Type(&types.NoneType{})
	if m.Name == "__init__" {
		retType = &types.NoneType{}
	} else if m.ReturnType != nil {
		retType = g.resolveTypeAnnotation(m.ReturnType)
	}
	retLLVM := g.llvmType(retType)

	selfLLVM := fmt.Sprintf("%%Class.%s*", ct.Name)
	params := []string{fmt.Sprintf("%s %%self", selfLLVM)}
	paramTypes := []types.Type{&types.InstanceType{Class: ct}}

	for i := 1; i < len(m.Params); i++ {
		p := m.Params[i]
		pType := g.resolveTypeAnnotation(p.TypeAnn)
		params = append(params, fmt.Sprintf("%s %%%s", g.llvmType(pType), p.Name))
		paramTypes = append(paramTypes, pType)
	}

	g.emitLine(fmt.Sprintf("define %s %s(%s) {", retLLVM, mangled, strings.Join(params, ", ")))
	g.emitLine("entry:")

	// Save and restore state.
	oldVars := g.vars
	oldInFunc := g.inFunction
	oldClass := g.currentClass
	oldRet := g.currentReturnType
	oldRetLLVM := g.currentReturnLLVMType
	g.vars = map[string]varInfo{}
	g.inFunction = true
	g.currentClass = ct
	g.currentReturnType = retType
	g.currentReturnLLVMType = retLLVM

	// Alloca for self and params.
	selfAlloca := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca %s", selfAlloca, selfLLVM))
	g.emitLine(fmt.Sprintf("  store %s %%self, %s* %s", selfLLVM, selfLLVM, selfAlloca))
	g.vars["self"] = varInfo{llvmName: selfAlloca, typ: &types.InstanceType{Class: ct}}

	for i := 1; i < len(m.Params); i++ {
		p := m.Params[i]
		pType := paramTypes[i]
		llvmT := g.llvmType(pType)
		allocaName := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = alloca %s", allocaName, llvmT))
		g.emitLine(fmt.Sprintf("  store %s %%%s, %s* %s", llvmT, p.Name, llvmT, allocaName))
		g.vars[p.Name] = varInfo{llvmName: allocaName, typ: pType}
	}

	for _, stmt := range m.Body {
		if err := g.emitStmt(stmt); err != nil {
			g.vars = oldVars
			g.inFunction = oldInFunc
			g.currentClass = oldClass
			g.currentReturnType = oldRet
			g.currentReturnLLVMType = oldRetLLVM
			return err
		}
	}

	if _, isNone := retType.(*types.NoneType); isNone {
		g.emitLine("  ret void")
	}
	g.emitLine("}")

	g.vars = oldVars
	g.inFunction = oldInFunc
	g.currentClass = oldClass
	g.currentReturnType = oldRet
	g.currentReturnLLVMType = oldRetLLVM
	return nil
}

// emitVTable emits the vtable global for a class. For each slot, the owning
// class's mangled method function is referenced.
func (g *Generator) emitVTable(ct *types.ClassType) {
	order := g.slotOrder[ct]
	owners := g.slotOwner[ct]

	basePart := "i8* null"
	if ct.Base != nil {
		basePart = fmt.Sprintf("i8* bitcast (%%VTable.%s* @vtable.%s to i8*)", ct.Base.Name, ct.Base.Name)
	}

	parts := []string{fmt.Sprintf("i32 %d", ct.ClassID), basePart}
	for i, mName := range order {
		owner := owners[i]
		// Build function-type cast string that matches the method's IR signature.
		fnType := g.methodFuncType(owner, mName)
		parts = append(parts, fmt.Sprintf("i8* bitcast (%s %s to i8*)", fnType, g.methodMangled(owner, mName)))
	}

	g.emitLine(fmt.Sprintf("@vtable.%s = global %%VTable.%s { %s }", ct.Name, ct.Name, strings.Join(parts, ", ")))
}

// methodFuncType returns the LLVM function-type prefix for a class method, in
// the form `<ret> (<self*>, <args...>)*` — suitable for a bitcast operand.
func (g *Generator) methodFuncType(ownerClass *types.ClassType, methodName string) string {
	sig, ok := ownerClass.Methods[methodName]
	if !ok {
		// Synthetic __str__/__repr__: signature is self -> i8*.
		return fmt.Sprintf("i8* (%%Class.%s*)*", ownerClass.Name)
	}
	retLLVM := g.llvmType(sig.Return)
	parts := []string{fmt.Sprintf("%%Class.%s*", ownerClass.Name)}
	for _, p := range sig.Params {
		parts = append(parts, g.llvmType(p))
	}
	return fmt.Sprintf("%s (%s)*", retLLVM, strings.Join(parts, ", "))
}

// emitSyntheticStr emits an auto-default __str__ (or __repr__) that produces a
// spy-string of the form: ClassName(field1=value1, field2=value2).
func (g *Generator) emitSyntheticStr(ct *types.ClassType, name string) {
	mangled := g.methodMangled(ct, name)
	selfLLVM := fmt.Sprintf("%%Class.%s*", ct.Name)

	g.emitLine(fmt.Sprintf("define i8* %s(%s %%self) {", mangled, selfLLVM))
	g.emitLine("entry:")

	// Register all the literal strings we'll need.
	openTag := ct.Name + "("
	g.addStringConst(openTag)
	g.addStringConst(")")
	g.addStringConst(", ")
	for _, f := range ct.Fields {
		g.addStringConst(f.Name + "=")
	}

	cur := g.spyLiteral(openTag)

	for i, f := range ct.Fields {
		if i > 0 {
			sep := g.spyLiteral(", ")
			next := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_concat(i8* %s, i8* %s)", next, cur, sep))
			cur = next
		}
		labelStr := g.spyLiteral(f.Name + "=")
		afterLabel := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_concat(i8* %s, i8* %s)", afterLabel, cur, labelStr))
		cur = afterLabel

		fieldPtr := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %%Class.%s* %%self, i32 0, i32 %d",
			fieldPtr, ct.Name, ct.Name, i+1))
		fieldLLVM := g.llvmType(f.Type)
		fieldVal := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", fieldVal, fieldLLVM, fieldLLVM, fieldPtr))

		valStr := g.spyValueToStr(f.Type, fieldVal)
		afterVal := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_concat(i8* %s, i8* %s)", afterVal, cur, valStr))
		cur = afterVal
	}

	closeTag := g.spyLiteral(")")
	final := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_concat(i8* %s, i8* %s)", final, cur, closeTag))
	g.emitLine(fmt.Sprintf("  ret i8* %s", final))
	g.emitLine("}")
}

// spyLiteral emits IR to allocate a spy-string for the given Go string and
// returns the LLVM SSA name holding the pointer.
func (g *Generator) spyLiteral(s string) string {
	g.addStringConst(s)
	idx := g.getStringIndex(s)
	tmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_new(i8* getelementptr ([%d x i8], [%d x i8]* @.str.%d, i64 0, i64 0), i64 %d)",
		tmp, len(s), len(s), idx, len(s)))
	return tmp
}

// spyValueToStr emits IR to convert a typed LLVM value into a spy-string and
// returns the resulting SSA name.
func (g *Generator) spyValueToStr(t types.Type, val string) string {
	result := g.newTmp()
	switch v := t.(type) {
	case *types.IntType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_int_to_str(i64 %s)", result, val))
	case *types.FloatType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_float_to_str(double %s)", result, val))
	case *types.BoolType:
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_bool_to_str(i1 %s)", result, val))
	case *types.StrType:
		return val
	case *types.InstanceType:
		// Dispatch through the instance's vtable for its __str__ slot.
		return g.emitVirtualCall(val, v.Class, "__str__", nil, nil, &types.StrType{})
	default:
		_ = v
		g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_new(i8* getelementptr ([1 x i8], [1 x i8]* @.str.0, i64 0, i64 0), i64 1)", result))
	}
	return result
}

// emitVirtualCall emits IR for a vtable-dispatched method call on receiver
// `selfVal` whose static type is `staticClass`. `methodName` is the method to
// call. `args`/`argTypes` are the remaining (non-self) arguments. `retType` is
// the method's return type. Returns the SSA name of the result (or "void" for
// void returns).
func (g *Generator) emitVirtualCall(selfVal string, staticClass *types.ClassType, methodName string, args []string, argTypes []types.Type, retType types.Type) string {
	slot, ok := g.methodSlots[staticClass][methodName]
	if !ok {
		// Method doesn't exist on this class — should have been caught by the checker.
		g.emitLine(fmt.Sprintf("  ; ERROR: method %s.%s has no slot", staticClass.Name, methodName))
		return "undef"
	}

	// Build the LLVM function-pointer type.
	fnTypePrefix := retLLVMOrVoid(g, retType)
	typeParams := []string{fmt.Sprintf("%%Class.%s*", staticClass.Name)}
	for _, at := range argTypes {
		typeParams = append(typeParams, g.llvmType(at))
	}
	fnTypeStr := fmt.Sprintf("%s (%s)", fnTypePrefix, strings.Join(typeParams, ", "))

	// Load vtable pointer (offset 0) from instance.
	selfLLVM := fmt.Sprintf("%%Class.%s*", staticClass.Name)
	vtabSlotPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %s %s, i32 0, i32 0",
		vtabSlotPtr, staticClass.Name, selfLLVM, selfVal))
	vtabGenericPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", vtabGenericPtr, vtabSlotPtr))
	// Cast to the static-class vtable type so GEP slot index matches.
	vtabTyped := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %%VTable.%s*",
		vtabTyped, vtabGenericPtr, staticClass.Name))
	// GEP to slot (offset 2 + slot in the vtable struct).
	slotPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr %%VTable.%s, %%VTable.%s* %s, i32 0, i32 %d",
		slotPtr, staticClass.Name, staticClass.Name, vtabTyped, slot+2))
	fnOpaque := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", fnOpaque, slotPtr))
	fnTyped := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %s*", fnTyped, fnOpaque, fnTypeStr))

	// Build arg list.
	allArgs := []string{fmt.Sprintf("%s %s", selfLLVM, selfVal)}
	for i, a := range args {
		allArgs = append(allArgs, fmt.Sprintf("%s %s", g.llvmType(argTypes[i]), a))
	}

	if _, isNone := retType.(*types.NoneType); isNone {
		g.emitLine(fmt.Sprintf("  call void %s(%s)", fnTyped, strings.Join(allArgs, ", ")))
		return "void"
	}
	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call %s %s(%s)", result, g.llvmType(retType), fnTyped, strings.Join(allArgs, ", ")))
	return result
}

// emitStaticCall emits a direct (non-virtual) call to a method — used for
// super().method(...). selfVal is the instance pointer; ownerClass is the
// defining class; args/argTypes are the non-self arguments.
func (g *Generator) emitStaticCall(selfVal string, selfClass *types.ClassType, ownerClass *types.ClassType, methodName string, args []string, argTypes []types.Type, retType types.Type) string {
	// If self's static class differs from ownerClass, we may need to bitcast.
	ownerLLVM := fmt.Sprintf("%%Class.%s*", ownerClass.Name)
	castedSelf := selfVal
	if selfClass != ownerClass {
		castedSelf = g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = bitcast %%Class.%s* %s to %s", castedSelf, selfClass.Name, selfVal, ownerLLVM))
	}

	mangled := g.methodMangled(ownerClass, methodName)
	allArgs := []string{fmt.Sprintf("%s %s", ownerLLVM, castedSelf)}
	for i, a := range args {
		allArgs = append(allArgs, fmt.Sprintf("%s %s", g.llvmType(argTypes[i]), a))
	}
	if _, isNone := retType.(*types.NoneType); isNone {
		g.emitLine(fmt.Sprintf("  call void %s(%s)", mangled, strings.Join(allArgs, ", ")))
		return "void"
	}
	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call %s %s(%s)", result, g.llvmType(retType), mangled, strings.Join(allArgs, ", ")))
	return result
}

// emitConstructorCall emits IR for `ClassName(args)`: allocates an instance,
// stores the vtable pointer, invokes __init__, and returns the instance pointer.
func (g *Generator) emitConstructorCall(ct *types.ClassType, args []parser.Expr) (string, error) {
	// Compute struct size: 8 bytes vtable ptr + sum of field sizes (rounded to 8).
	size := int64(8)
	for _, f := range ct.Fields {
		// Align each field to 8 bytes; it's a conservative over-allocation but safe.
		size += int64(g.fieldAllocSize(f.Type))
	}

	rawPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_instance_new(i64 %d)", rawPtr, size))
	instPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %%Class.%s*", instPtr, rawPtr, ct.Name))

	// Store vtable pointer in slot 0.
	vtabSlotPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %%Class.%s* %s, i32 0, i32 0",
		vtabSlotPtr, ct.Name, ct.Name, instPtr))
	vtabGeneric := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %%VTable.%s* @vtable.%s to i8*", vtabGeneric, ct.Name, ct.Name))
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", vtabGeneric, vtabSlotPtr))

	// Emit arguments for __init__.
	if _, hasInit := ct.Methods["__init__"]; hasInit || findInitOwner(ct) != nil {
		owner := findInitOwner(ct)
		if owner == nil {
			owner = ct
		}
		argVals := []string{}
		argTypes := []types.Type{}
		for _, arg := range args {
			val, err := g.emitExpr(arg)
			if err != nil {
				return "", err
			}
			argVals = append(argVals, val)
			argTypes = append(argTypes, arg.GetResolvedType().(types.Type))
		}
		g.emitStaticCall(instPtr, ct, owner, "__init__", argVals, argTypes, &types.NoneType{})
	}

	return instPtr, nil
}

// findInitOwner walks up the base chain to find which class defines __init__.
func findInitOwner(ct *types.ClassType) *types.ClassType {
	for c := ct; c != nil; c = c.Base {
		if _, ok := c.Methods["__init__"]; ok {
			// Methods includes inherited; check only OwnMethods
			if c.OwnMethods != nil && c.OwnMethods["__init__"] {
				return c
			}
		}
	}
	// Walk again but also accept inherited __init__ presence
	for c := ct; c != nil; c = c.Base {
		if c.OwnMethods != nil && c.OwnMethods["__init__"] {
			return c
		}
	}
	return nil
}

// fieldAllocSize returns the number of bytes a field occupies in the struct
// layout. Used for spy_instance_new sizing.
func (g *Generator) fieldAllocSize(t types.Type) int {
	switch t.(type) {
	case *types.IntType, *types.FloatType:
		return 8
	case *types.BoolType:
		return 8 // over-allocate for simple alignment
	case *types.StrType, *types.ListType, *types.MapType, *types.InstanceType:
		return 8
	}
	return 8
}

// emitIsInstance emits IR for `isinstance(obj, Class)`: walks the vtable chain
// comparing class_id at each level. Returns an SSA name holding the i1 result.
func (g *Generator) emitIsInstance(objVal string, objClass *types.ClassType, target *types.ClassType) string {
	// Load initial vtable pointer from obj.
	vtabSlotPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %%Class.%s* %s, i32 0, i32 0",
		vtabSlotPtr, objClass.Name, objClass.Name, objVal))
	startVtab := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", startVtab, vtabSlotPtr))

	// Alloca for current vtable pointer and result.
	curAlloca := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca i8*", curAlloca))
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", startVtab, curAlloca))
	resultAlloca := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = alloca i1", resultAlloca))
	g.emitLine(fmt.Sprintf("  store i1 0, i1* %s", resultAlloca))

	loopLabel := g.newLabel("isinstance.loop")
	matchLabel := g.newLabel("isinstance.match")
	nextLabel := g.newLabel("isinstance.next")
	endLabel := g.newLabel("isinstance.end")

	g.emitLine(fmt.Sprintf("  br label %%%s", loopLabel))
	g.emitLine(fmt.Sprintf("%s:", loopLabel))

	cur := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", cur, curAlloca))
	isNull := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp eq i8* %s, null", isNull, cur))
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", isNull, endLabel, matchLabel))

	g.emitLine(fmt.Sprintf("%s:", matchLabel))
	// Load class_id at offset 0 of the vtable (class_id is i32 at offset 0).
	// We don't know the specific VTable type, so use a generic cast to {i32, i8*}.
	genVtab := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to { i32, i8* }*", genVtab, cur))
	idPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr { i32, i8* }, { i32, i8* }* %s, i32 0, i32 0", idPtr, genVtab))
	idVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i32, i32* %s", idVal, idPtr))
	cmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp eq i32 %s, %d", cmp, idVal, target.ClassID))
	foundLabel := g.newLabel("isinstance.found")
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, foundLabel, nextLabel))

	g.emitLine(fmt.Sprintf("%s:", foundLabel))
	g.emitLine(fmt.Sprintf("  store i1 1, i1* %s", resultAlloca))
	g.emitLine(fmt.Sprintf("  br label %%%s", endLabel))

	g.emitLine(fmt.Sprintf("%s:", nextLabel))
	// Load base vtable pointer (offset 1).
	basePtrPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr { i32, i8* }, { i32, i8* }* %s, i32 0, i32 1", basePtrPtr, genVtab))
	baseVtab := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i8*, i8** %s", baseVtab, basePtrPtr))
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", baseVtab, curAlloca))
	g.emitLine(fmt.Sprintf("  br label %%%s", loopLabel))

	g.emitLine(fmt.Sprintf("%s:", endLabel))
	result := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load i1, i1* %s", result, resultAlloca))
	return result
}

// retLLVMOrVoid returns "void" for NoneType else the llvm type. Used inside a
// function-pointer type string builder.
func retLLVMOrVoid(g *Generator, t types.Type) string {
	if _, ok := t.(*types.NoneType); ok {
		return "void"
	}
	return g.llvmType(t)
}

// binaryOpDunder maps a binary operator token to its dunder method name.
func binaryOpDunder(op string) string {
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

// printInstance emits IR that converts an instance to a spy-string via its
// __str__ vtable slot and prints it with spy_print_str.
func (g *Generator) printInstance(val string, inst *types.InstanceType) {
	str := g.emitVirtualCall(val, inst.Class, "__str__", nil, nil, &types.StrType{})
	g.emitLine(fmt.Sprintf("  call void @spy_print_str(i8* %s)", str))
}

// emitInstanceMethodCall emits a virtual method call on `attr.Object`.
func (g *Generator) emitInstanceMethodCall(attr *parser.AttrExpr, inst *types.InstanceType, args []parser.Expr) (string, error) {
	selfVal, err := g.emitExpr(attr.Object)
	if err != nil {
		return "", err
	}
	sig, ok := inst.Class.Methods[attr.Attr]
	if !ok {
		return "", fmt.Errorf("%s has no method %s", inst.Class.Name, attr.Attr)
	}
	argVals := []string{}
	argTypes := []types.Type{}
	for i, a := range args {
		val, err := g.emitExpr(a)
		if err != nil {
			return "", err
		}
		at := a.GetResolvedType().(types.Type)
		// Upcast to the method's declared param type if necessary.
		if i < len(sig.Params) {
			val = g.castToType(val, at, sig.Params[i])
			at = sig.Params[i]
		}
		argVals = append(argVals, val)
		argTypes = append(argTypes, at)
	}
	return g.emitVirtualCall(selfVal, inst.Class, attr.Attr, argVals, argTypes, sig.Return), nil
}

// emitSuperCall handles `super().method(args)` — a direct call to the base
// class's method function (non-virtual).
func (g *Generator) emitSuperCall(attr *parser.AttrExpr, args []parser.Expr) (string, error) {
	if g.currentClass == nil {
		return "", fmt.Errorf("super() used outside a class method")
	}
	base := g.currentClass.Base
	if base == nil {
		return "", fmt.Errorf("class %s has no base", g.currentClass.Name)
	}
	// Locate the defining class for the method (might be further up the chain).
	owner := findMethodOwner(base, attr.Attr)
	if owner == nil {
		return "", fmt.Errorf("base class %s has no method %s", base.Name, attr.Attr)
	}

	// Load self from local alloca.
	selfInfo, ok := g.vars["self"]
	if !ok {
		return "", fmt.Errorf("super() requires self in scope")
	}
	selfLLVM := g.llvmType(selfInfo.typ)
	selfVal := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", selfVal, selfLLVM, selfLLVM, selfInfo.llvmName))

	// Look up the method's signature.
	var sig *types.FuncType
	if attr.Attr == "__init__" {
		// __init__ may not be in Methods map depending on how stored; use base's __init__ signature.
		if s, ok := owner.Methods["__init__"]; ok {
			sig = s
		} else {
			sig = &types.FuncType{Params: []types.Type{}, Return: &types.NoneType{}}
		}
	} else {
		sig = owner.Methods[attr.Attr]
	}
	if sig == nil {
		sig = &types.FuncType{Params: []types.Type{}, Return: &types.NoneType{}}
	}

	argVals := []string{}
	argTypes := []types.Type{}
	for i, a := range args {
		val, err := g.emitExpr(a)
		if err != nil {
			return "", err
		}
		at := a.GetResolvedType().(types.Type)
		if i < len(sig.Params) {
			val = g.castToType(val, at, sig.Params[i])
			at = sig.Params[i]
		}
		argVals = append(argVals, val)
		argTypes = append(argTypes, at)
	}
	return g.emitStaticCall(selfVal, g.currentClass, owner, attr.Attr, argVals, argTypes, sig.Return), nil
}

// findMethodOwner locates the class in ct's ancestry (including ct itself)
// that defines `methodName` in its OwnMethods.
func findMethodOwner(ct *types.ClassType, methodName string) *types.ClassType {
	for c := ct; c != nil; c = c.Base {
		if c.OwnMethods != nil && c.OwnMethods[methodName] {
			return c
		}
	}
	return nil
}

// emitIsInstanceCall emits IR for `isinstance(obj, Class)`.
func (g *Generator) emitIsInstanceCall(e *parser.CallExpr) (string, error) {
	if len(e.Args) != 2 {
		return "", fmt.Errorf("isinstance() takes 2 arguments, got %d", len(e.Args))
	}
	objVal, err := g.emitExpr(e.Args[0])
	if err != nil {
		return "", err
	}
	objT, ok := e.Args[0].GetResolvedType().(*types.InstanceType)
	if !ok {
		return "", fmt.Errorf("isinstance() first argument must be an instance")
	}
	target, ok := e.Args[1].GetResolvedType().(*types.ClassType)
	if !ok {
		return "", fmt.Errorf("isinstance() second argument must be a class")
	}
	return g.emitIsInstance(objVal, objT.Class, target), nil
}
