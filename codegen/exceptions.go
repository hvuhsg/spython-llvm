package codegen

import (
	"fmt"
	"strings"

	"github.com/yehoyadashtinmetz/spython/parser"
	"github.com/yehoyadashtinmetz/spython/types"
)

// Pending-action encoding for try/finally control flow. When a try has a
// finally block, break/continue/return inside the try body write to the
// pending slot and branch to the finally entry; the finally's trailing
// switch then dispatches to the real exit.
const (
	pendingFallthrough = 0
	pendingRethrow     = 1
	pendingReturn      = 2
	pendingBreak       = 3
	pendingContinue    = 4
)

// finallyFrame describes one `try ... finally` currently open. Multiple
// frames live on g.finallyStack (innermost last). The fields that end in
// Target are precomputed by pushFinallyFrame so the finally's switch can
// chain to an outer finally when a break/continue/return needs to cross
// several nested trys to reach its real target.
type finallyFrame struct {
	// entryLabel is the basic-block label of the finally body.
	// break/continue/return inside the try branch here after writing pending.
	entryLabel string
	// pendingSlot is an alloca'd i32*.
	pendingSlot string
	// retSlot is an alloca'd <RetType>* — empty for void-returning functions.
	retSlot     string
	retLLVMType string // "" for void
	// These are the switch's dispatch targets (pre-resolved for chaining):
	returnTarget string // label emitting the real `ret`
	breakTarget  string // label of outer break (real or an outer finally's entry)
	contTarget   string // label of outer continue (real or an outer finally's entry)
	rethrowLabel string // label that calls spy_exc_rethrow
	// outerBreakSnapshot is the innermost real break label that was active
	// when this try was pushed; used to recognize whether a break inside the
	// try targets outside the try or a loop nested within.
	outerBreakSnapshot string
	outerContSnapshot  string
}

// emitFloatDivZeroCheck emits a branch that raises ZeroDivisionError if rhs
// is zero. LLVM's IEEE 754 semantics would otherwise return inf/nan on
// division/modulo by zero, but Python raises ZeroDivisionError in that case —
// we match CPython. Messages mirror CPython's.
func (g *Generator) emitFloatDivZeroCheck(rhsVal, op string) {
	var msgText string
	switch op {
	case "/":
		msgText = "float division by zero"
	case "//":
		msgText = "float floor division by zero"
	case "%":
		msgText = "float modulo"
	default:
		msgText = "float division by zero"
	}
	g.emitDivZeroCheck(rhsVal, "double", "fcmp oeq", msgText)
}

// emitIntDivZeroCheck emits a branch that raises ZeroDivisionError if rhs is
// zero. Falls through on the non-zero path. Called from integer `/`, `//`,
// and `%` lowering. rhsVal is an i64 SSA name; op is "/", "//", or "%" and
// feeds into the exception message so the error text is actionable.
//
// The ZeroDivisionError class is injected into every entry module by the
// loader (loader/builtins.go), so it is guaranteed to be registered.
func (g *Generator) emitIntDivZeroCheck(rhsVal, op string) {
	msgText := "integer division by zero"
	if op == "%" {
		msgText = "integer modulo by zero"
	}
	g.emitDivZeroCheck(rhsVal, "i64", "icmp eq", msgText)
}

// emitDivZeroCheck is the shared guard used by both integer and float paths.
// rhsLLVMType is "i64" or "double"; cmpOp is "icmp eq" or "fcmp oeq"; zeroLit
// is derived from rhsLLVMType.
func (g *Generator) emitDivZeroCheck(rhsVal, rhsLLVMType, cmpOp, msgText string) {
	ct, ok := g.classByName["ZeroDivisionError"]
	if !ok {
		return
	}
	zeroLit := "0"
	if rhsLLVMType == "double" {
		zeroLit = "0.0"
	}

	isZero := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = %s %s %s, %s", isZero, cmpOp, rhsLLVMType, rhsVal, zeroLit))
	throwLbl := g.newLabel("divzero.throw")
	okLbl := g.newLabel("divzero.ok")
	g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", isZero, throwLbl, okLbl))

	g.emitLine(fmt.Sprintf("%s:", throwLbl))
	msgIdx := g.getStringIndex(msgText)
	msgSSA := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_str_new(i8* getelementptr ([%d x i8], [%d x i8]* @.str.%d, i64 0, i64 0), i64 %d)",
		msgSSA, len(msgText), len(msgText), msgIdx, len(msgText)))
	inst, err := g.emitSyntheticConstructor(ct, []syntheticArg{{llvmType: "i8*", val: msgSSA}})
	if err != nil {
		panic(err)
	}
	rawInst := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %%Class.%s* %s to i8*", rawInst, ct.Name, inst))
	g.emitLine(fmt.Sprintf("  call void @spy_exc_throw(i8* %s)", rawInst))
	g.emitLine("  unreachable")

	g.emitLine(fmt.Sprintf("%s:", okLbl))
}

// syntheticArg is a pre-computed constructor argument used by
// emitSyntheticConstructor — bypasses emitExpr so codegen-internal callers
// (like the divide-by-zero guard) don't need a real parser.Expr tree.
type syntheticArg struct {
	llvmType string // e.g. "i8*"
	val      string // SSA name
}

// emitSyntheticConstructor is a variant of emitConstructorCall that takes
// pre-computed LLVM SSA values instead of parser.Exprs. It runs the normal
// vtable wiring and __init__ dispatch so the resulting instance is
// indistinguishable from one created by user code.
func (g *Generator) emitSyntheticConstructor(ct *types.ClassType, args []syntheticArg) (string, error) {
	size := int64(8)
	for _, f := range ct.Fields {
		size += int64(g.fieldAllocSize(f.Type))
	}

	rawPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_instance_new(i64 %d)", rawPtr, size))
	instPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %%Class.%s*", instPtr, rawPtr, ct.Name))

	vtabSlotPtr := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = getelementptr %%Class.%s, %%Class.%s* %s, i32 0, i32 0",
		vtabSlotPtr, ct.Name, ct.Name, instPtr))
	vtabGeneric := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %%VTable.%s* @vtable.%s to i8*",
		vtabGeneric, ct.Name, ct.Name))
	g.emitLine(fmt.Sprintf("  store i8* %s, i8** %s", vtabGeneric, vtabSlotPtr))

	// Call __init__ via the class method mangling. methodMangled returns
	// the "@"-prefixed LLVM global name.
	fnName := g.methodMangled(ct, "__init__")
	argStrs := []string{fmt.Sprintf("%%Class.%s* %s", ct.Name, instPtr)}
	for _, a := range args {
		argStrs = append(argStrs, fmt.Sprintf("%s %s", a.llvmType, a.val))
	}
	g.emitLine(fmt.Sprintf("  call void %s(%s)", fnName, strings.Join(argStrs, ", ")))

	return instPtr, nil
}

// emitRaiseStmt: lower `raise <expr>` to spy_exc_throw.
func (g *Generator) emitRaiseStmt(s *parser.RaiseStmt) error {
	v, err := g.emitExpr(s.Value)
	if err != nil {
		return err
	}
	inst, ok := s.Value.GetResolvedType().(*types.InstanceType)
	if !ok {
		return fmt.Errorf("raise: resolved type is not an instance: %T", s.Value.GetResolvedType())
	}
	raw := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = bitcast %%Class.%s* %s to i8*", raw, inst.Class.Name, v))
	g.emitLine(fmt.Sprintf("  call void @spy_exc_throw(i8* %s)", raw))
	g.emitLine("  unreachable")
	// Emit a dead label so subsequent code (if any) has a well-formed block.
	dead := g.newLabel("after.raise")
	g.emitLine(fmt.Sprintf("%s:", dead))
	return nil
}

// emitTryStmt lowers a full try/except/finally. Layout:
//
//   <allocas>
//   spy_exc_push(buf); sj = setjmp(buf)
//   br (sj == 0) body, dispatch
//
// body:
//   <try body, where break/continue/return route through finally if present>
//   spy_exc_pop()
//   <br to finally.entry or try.end>
//
// dispatch:
//   spy_exc_pop()             ; so a re-raise hits the parent handler
//   exc = spy_exc_current()
//   <per-clause classID match + bound-var binding + body; on match spy_exc_clear
//    then branch to finally.entry or try.end; final fall-through rethrows>
//
// finally (if present):
//   <finally body>
//   switch pending -> end / rethrow / return / break / continue
//
// end: normal fall-through.
func (g *Generator) emitTryStmt(s *parser.TryStmt) error {
	// Allocate the jmp_buf / frame. We oversize to 256 bytes to cover any
	// platform's jmp_buf plus the linked-list `prev` pointer. The runtime
	// static-asserts sizeof(SpyExcFrame) <= 256.
	bufArr := g.newTmp()
	g.emitAlloca(bufArr, "[256 x i8]")
	bufI8 := g.newTmp()
	// Hoist the jmp_buf bitcast into the entry block so it dominates every
	// use. In a generator's __next__, a try region that spans a yield (e.g.
	// `yield from`) is re-entered through the dispatch switch, which bypasses
	// the try-setup block; an inline bitcast there would not dominate the
	// loop head that runs setjmp.
	g.emitEntry(fmt.Sprintf("  %s = bitcast [256 x i8]* %s to i8*", bufI8, bufArr))

	tryBody := g.newLabel("try.body")
	dispatch := g.newLabel("try.dispatch")
	tryEnd := g.newLabel("try.end")

	// With a finally, a single handler stays pushed across BOTH the try body
	// and the except bodies; it is popped exactly once at finally.entry. A
	// phase flag (0 = in try body, 1 = in an except handler body) lets the
	// shared setjmp landing decide where a throw should go: dispatch (run the
	// clause checks) versus finally-then-rethrow (a handler itself raised).
	// This is what makes `finally` run when an except handler exits via
	// `return`/`break`/`continue` (routed through finallyStack) or via `raise`
	// (routed through the phase==1 landing).
	var phaseSlot, onThrow, handlerEscaped string
	if s.HasFinally {
		onThrow = g.newLabel("try.onthrow")
		handlerEscaped = g.newLabel("try.handler.escaped")
	}

	// Finally infrastructure — only if this try has a finally clause.
	var frame finallyFrame
	hasFinally := s.HasFinally
	if hasFinally {
		frame.entryLabel = g.newLabel("try.finally")
		frame.returnTarget = g.newLabel("try.doreturn")
		frame.rethrowLabel = g.newLabel("try.rethrow")

		// Allocate pending slot and (if needed) ret slot.
		frame.pendingSlot = g.newTmp()
		g.emitAlloca(frame.pendingSlot, "i32")
		g.emitLine(fmt.Sprintf("  store i32 %d, i32* %s", pendingFallthrough, frame.pendingSlot))

		// Phase slot: 0 in the try body, 1 in an except handler body. Its value
		// is written in the dispatch block and must survive the longjmp that a
		// re-raise in a handler triggers. Accesses are `volatile` so the
		// optimizer cannot promote the slot to a register (which longjmp would
		// clobber) — the classic setjmp/longjmp non-volatile-local hazard.
		phaseSlot = g.newTmp()
		g.emitAlloca(phaseSlot, "i32")
		g.emitLine(fmt.Sprintf("  store volatile i32 0, i32* %s", phaseSlot))

		if g.currentReturnLLVMType != "" && g.currentReturnLLVMType != "void" {
			frame.retLLVMType = g.currentReturnLLVMType
			frame.retSlot = g.newTmp()
			g.emitAlloca(frame.retSlot, frame.retLLVMType)
		}

		// Snapshots + break/continue targets used by the finally switch.
		if len(g.breakLabels) > 0 {
			frame.outerBreakSnapshot = g.breakLabels[len(g.breakLabels)-1]
		}
		if len(g.continueLabels) > 0 {
			frame.outerContSnapshot = g.continueLabels[len(g.continueLabels)-1]
		}
		// If an outer finally wraps the same break/continue target, route
		// through that outer finally's entry; otherwise branch directly.
		frame.breakTarget = g.resolveExitTarget(frame.outerBreakSnapshot, breakKind)
		frame.contTarget = g.resolveExitTarget(frame.outerContSnapshot, continueKind)
	}

	// Push frame / run setjmp.
	g.emitLine(fmt.Sprintf("  call void @spy_exc_push(i8* %s)", bufI8))
	sj := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i32 @setjmp(i8* %s)", sj, bufI8))
	cmp := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = icmp eq i32 %s, 0", cmp, sj))
	if hasFinally {
		// A throw lands here; onThrow decides dispatch vs finally-rethrow.
		g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, tryBody, onThrow))
	} else {
		g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cmp, tryBody, dispatch))
	}

	// ---- onThrow (finally only): route by phase ----
	if hasFinally {
		g.emitLine(fmt.Sprintf("%s:", onThrow))
		ph := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load volatile i32, i32* %s", ph, phaseSlot))
		inTry := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = icmp eq i32 %s, 0", inTry, ph))
		// phase 0 → throw from try body → run clause checks; phase 1 → throw
		// escaped an except handler → run finally, then rethrow to parent.
		g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", inTry, dispatch, handlerEscaped))

		g.emitLine(fmt.Sprintf("%s:", handlerEscaped))
		g.emitLine(fmt.Sprintf("  store i32 %d, i32* %s", pendingRethrow, frame.pendingSlot))
		g.emitLine(fmt.Sprintf("  br label %%%s", frame.entryLabel))
	}

	// ---- Try body ----
	g.emitLine(fmt.Sprintf("%s:", tryBody))
	if hasFinally {
		g.finallyStack = append(g.finallyStack, frame)
	}
	for _, st := range s.Body {
		if err := g.emitStmt(st); err != nil {
			if hasFinally {
				g.finallyStack = g.finallyStack[:len(g.finallyStack)-1]
			}
			return err
		}
	}
	if hasFinally {
		g.finallyStack = g.finallyStack[:len(g.finallyStack)-1]
	}
	// Normal completion. Without a finally, pop the handler and go to the end.
	// With a finally, the handler stays pushed and is popped once in
	// finally.entry, so just branch there.
	if hasFinally {
		g.emitLine(fmt.Sprintf("  br label %%%s", frame.entryLabel))
	} else {
		g.emitLine("  call void @spy_exc_pop()")
		g.emitLine(fmt.Sprintf("  br label %%%s", tryEnd))
	}

	// ---- Dispatch ----
	g.emitLine(fmt.Sprintf("%s:", dispatch))
	if hasFinally {
		// Handler stays pushed (popped in finally.entry). Mark phase=1 so a
		// throw from any handler body lands at handlerEscaped, not dispatch.
		g.emitLine(fmt.Sprintf("  store volatile i32 1, i32* %s", phaseSlot))
	} else {
		// Pop before running except bodies, so a re-raise in an except clause
		// propagates to the parent handler instead of this same try's buffer.
		g.emitLine("  call void @spy_exc_pop()")
	}
	excRaw := g.newTmp()
	g.emitLine(fmt.Sprintf("  %s = call i8* @spy_exc_current()", excRaw))

	// Chain clause checks: each typed clause emits an isinstance test; on
	// match it binds the variable, clears in-flight, runs the body, and
	// branches to finally/end. Bare except always matches.
	for i, ec := range s.Excepts {
		matchLbl := g.newLabel(fmt.Sprintf("try.catch.%d.match", i))
		nextLbl := g.newLabel(fmt.Sprintf("try.catch.%d.next", i))

		if ec.ExcType == nil {
			// Bare except: always match.
			g.emitLine(fmt.Sprintf("  br label %%%s", matchLbl))
		} else {
			t := g.resolveTypeAnnotation(ec.ExcType)
			inst, ok := t.(*types.InstanceType)
			if !ok {
				return fmt.Errorf("except clause type %q did not resolve to a class", ec.ExcType.Name)
			}
			cond := g.emitIsInstanceRaw(excRaw, inst.Class.ClassID)
			g.emitLine(fmt.Sprintf("  br i1 %s, label %%%s, label %%%s", cond, matchLbl, nextLbl))
		}

		g.emitLine(fmt.Sprintf("%s:", matchLbl))

		// Bind the except variable, if any.
		var savedVar *varInfo
		var hadVar bool
		if ec.VarName != "" && ec.ExcType != nil {
			t := g.resolveTypeAnnotation(ec.ExcType)
			inst := t.(*types.InstanceType)
			className := inst.Class.Name
			// Cast raw exc to the proper class pointer type.
			castedPtr := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = bitcast i8* %s to %%Class.%s*", castedPtr, excRaw, className))
			// Alloca a slot for the variable so subsequent loads/stores work.
			alloca := g.newTmp()
			g.emitAlloca(alloca, fmt.Sprintf("%%Class.%s*", className))
			g.emitLine(fmt.Sprintf("  store %%Class.%s* %s, %%Class.%s** %s",
				className, castedPtr, className, alloca))
			if existing, ok := g.vars[ec.VarName]; ok {
				saved := existing
				savedVar = &saved
				hadVar = true
			}
			g.vars[ec.VarName] = varInfo{llvmName: alloca, typ: inst}
		}

		// Clear in-flight before running the handler. If the handler raises
		// a new exception, spy_exc_throw will overwrite inflight anyway.
		g.emitLine("  call void @spy_exc_clear()")

		// Run except body. With a finally, push this frame so that a
		// break/continue/return inside the handler routes through our finally
		// (the handler is still pushed and gets popped in finally.entry). A
		// `raise` inside the handler longjmps to our shared setjmp, lands at
		// onThrow with phase==1, and is routed to finally too. Without a
		// finally, exits route to the outer finally (if any), as before.
		if hasFinally {
			g.finallyStack = append(g.finallyStack, frame)
		}
		for _, st := range ec.Body {
			if err := g.emitStmt(st); err != nil {
				if hasFinally {
					g.finallyStack = g.finallyStack[:len(g.finallyStack)-1]
				}
				return err
			}
		}
		if hasFinally {
			g.finallyStack = g.finallyStack[:len(g.finallyStack)-1]
		}

		// Restore/unbind the variable.
		if ec.VarName != "" {
			if hadVar {
				g.vars[ec.VarName] = *savedVar
			} else {
				delete(g.vars, ec.VarName)
			}
		}

		// Handler completed normally: go to finally (or end).
		if hasFinally {
			g.emitLine(fmt.Sprintf("  br label %%%s", frame.entryLabel))
		} else {
			g.emitLine(fmt.Sprintf("  br label %%%s", tryEnd))
		}

		g.emitLine(fmt.Sprintf("%s:", nextLbl))
	}

	// No clause matched — propagate. If we have a finally, go there with
	// pending=rethrow; otherwise rethrow directly.
	if hasFinally {
		g.emitLine(fmt.Sprintf("  store i32 %d, i32* %s", pendingRethrow, frame.pendingSlot))
		g.emitLine(fmt.Sprintf("  br label %%%s", frame.entryLabel))
	} else {
		g.emitLine("  call void @spy_exc_rethrow()")
		g.emitLine("  unreachable")
	}

	// ---- Finally (if present) ----
	if hasFinally {
		g.emitLine(fmt.Sprintf("%s:", frame.entryLabel))
		// Pop the (single) handler that stayed pushed across the try body and
		// the except bodies. Every edge into finally.entry — normal completion,
		// no-match, return/break/continue routing, and a handler that raised —
		// arrives with this handler still on the stack, so it is popped exactly
		// once here. After this, spy_exc_rethrow (the pendingRethrow arm) and
		// any throw from the finally body propagate to the parent handler.
		g.emitLine("  call void @spy_exc_pop()")
		// Finally body — checker has already rejected break/continue/return
		// inside, so no pending dance is needed here.
		for _, st := range s.FinallyBody {
			if err := g.emitStmt(st); err != nil {
				return err
			}
		}
		// Switch on pending. Fallthrough (0) is the default → try.end.
		// Only emit cases for actions that have a meaningful target. If the
		// try is at module scope with no enclosing loop, for example, the
		// break/continue cases are unreachable (checker guarantees break/
		// continue only appear inside a loop), so we omit them rather than
		// emit `label %` with an empty name.
		p := g.newTmp()
		g.emitLine(fmt.Sprintf("  %s = load i32, i32* %s", p, frame.pendingSlot))
		cases := fmt.Sprintf("i32 %d, label %%%s", pendingRethrow, frame.rethrowLabel)
		cases += fmt.Sprintf(" i32 %d, label %%%s", pendingReturn, frame.returnTarget)
		if frame.breakTarget != "" {
			cases += fmt.Sprintf(" i32 %d, label %%%s", pendingBreak, frame.breakTarget)
		}
		if frame.contTarget != "" {
			cases += fmt.Sprintf(" i32 %d, label %%%s", pendingContinue, frame.contTarget)
		}
		g.emitLine(fmt.Sprintf("  switch i32 %s, label %%%s [ %s ]", p, tryEnd, cases))

		// Rethrow arm.
		g.emitLine(fmt.Sprintf("%s:", frame.rethrowLabel))
		g.emitLine("  call void @spy_exc_rethrow()")
		g.emitLine("  unreachable")

		// Return arm — load retslot and emit a real ret.
		g.emitLine(fmt.Sprintf("%s:", frame.returnTarget))
		if frame.retLLVMType == "" {
			// Void return: just `ret void`. This happens when the
			// function's return type is None.
			if g.currentReturnLLVMType == "void" {
				g.emitLine("  ret void")
			} else {
				// Shouldn't happen — finally without retslot in a non-void
				// function means no return was ever routed through here, so
				// emit unreachable.
				g.emitLine("  unreachable")
			}
		} else {
			rv := g.newTmp()
			g.emitLine(fmt.Sprintf("  %s = load %s, %s* %s", rv, frame.retLLVMType, frame.retLLVMType, frame.retSlot))
			g.emitLine(fmt.Sprintf("  ret %s %s", frame.retLLVMType, rv))
		}
	}

	// ---- End ----
	g.emitLine(fmt.Sprintf("%s:", tryEnd))
	return nil
}

type exitKind int

const (
	breakKind exitKind = iota
	continueKind
	returnKind
)

// resolveExitTarget: given the real outer exit label (or ""), walk the
// finallyStack to find any finally that lies BETWEEN the current point and
// that real target — if one exists, the "effective" target is that finally's
// entry (the innermost such). Since pushFinally's called BEFORE the new
// frame is appended, the stack passed here is the CURRENT stack (not
// including the new frame being built).
//
// Used when computing breakTarget/contTarget for a newly-pushed frame: the
// frame's own switch jumps to this label, which correctly chains through
// outer finallies.
func (g *Generator) resolveExitTarget(realLabel string, _ exitKind) string {
	// No outer frames — use the real label directly.
	if len(g.finallyStack) == 0 {
		return realLabel
	}
	// The innermost outer finally: if it was pushed while the same real
	// label was active (meaning the exit target is OUTSIDE that finally),
	// route through it. Its own switch will then route further if needed.
	outer := g.finallyStack[len(g.finallyStack)-1]
	// A finally's entry is the right intermediate target whenever the
	// outer finally wraps the real destination. We detect that by matching
	// the outer's snapshot: for break, outer.outerBreakSnapshot == realLabel
	// means the outer was pushed while `realLabel` was the innermost break.
	// For continue, the same with outerContSnapshot. For return, always
	// outer wraps (since return's "real target" is the function exit and
	// every finally wraps it).
	// But we call this from pushFinallyFrame with just a label, so the
	// caller passes the right snapshot label. Routing rule: if the outer
	// frame's same-kind snapshot equals realLabel, we route through outer.
	// Simpler conservative rule (single-pass, correct for the common case):
	// always route through the outer finally. This is safe because outer's
	// switch will either dispatch to its own outer finally or to the real
	// label.
	return outer.entryLabel
}

// routeExit is called by emitBreak/Continue/Return when finallyStack is
// non-empty. It writes the pending code (and optionally the retval) then
// branches to the innermost finally's entry.
//
// Returns true if routing happened; false means the caller should emit the
// normal br/ret.
func (g *Generator) routeExit(kind exitKind, retVal, retLLVM string) bool {
	if len(g.finallyStack) == 0 {
		return false
	}
	frame := g.finallyStack[len(g.finallyStack)-1]
	var code int
	switch kind {
	case breakKind:
		// Only route if the break target is outside THIS try. If a loop was
		// entered after the try was pushed (frame.outerBreakSnapshot differs
		// from the current innermost break label), the break belongs to the
		// inner loop and we should NOT route through finally.
		var curBreak string
		if len(g.breakLabels) > 0 {
			curBreak = g.breakLabels[len(g.breakLabels)-1]
		}
		if curBreak != frame.outerBreakSnapshot {
			return false
		}
		code = pendingBreak
	case continueKind:
		var curCont string
		if len(g.continueLabels) > 0 {
			curCont = g.continueLabels[len(g.continueLabels)-1]
		}
		if curCont != frame.outerContSnapshot {
			return false
		}
		code = pendingContinue
	case returnKind:
		code = pendingReturn
		if retLLVM != "" && retLLVM != "void" && frame.retSlot != "" {
			g.emitLine(fmt.Sprintf("  store %s %s, %s* %s",
				retLLVM, retVal, retLLVM, frame.retSlot))
		}
	}
	g.emitLine(fmt.Sprintf("  store i32 %d, i32* %s", code, frame.pendingSlot))
	g.emitLine(fmt.Sprintf("  br label %%%s", frame.entryLabel))
	return true
}
