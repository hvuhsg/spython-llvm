package types

import (
	"fmt"
	"strings"

	"github.com/yehoyadashtinmetz/spython/parser"
)

type Type interface {
	String() string
	Equals(Type) bool
}

type IntType struct{}

func (t *IntType) String() string    { return "int" }
func (t *IntType) Equals(o Type) bool { _, ok := o.(*IntType); return ok }

type FloatType struct{}

func (t *FloatType) String() string    { return "float" }
func (t *FloatType) Equals(o Type) bool { _, ok := o.(*FloatType); return ok }

type BoolType struct{}

func (t *BoolType) String() string    { return "bool" }
func (t *BoolType) Equals(o Type) bool { _, ok := o.(*BoolType); return ok }

type StrType struct{}

func (t *StrType) String() string    { return "str" }
func (t *StrType) Equals(o Type) bool { _, ok := o.(*StrType); return ok }

type BytesType struct{}

func (t *BytesType) String() string    { return "bytes" }
func (t *BytesType) Equals(o Type) bool { _, ok := o.(*BytesType); return ok }

type BytearrayType struct{}

func (t *BytearrayType) String() string    { return "bytearray" }
func (t *BytearrayType) Equals(o Type) bool { _, ok := o.(*BytearrayType); return ok }

type NoneType struct{}

func (t *NoneType) String() string    { return "None" }
func (t *NoneType) Equals(o Type) bool { _, ok := o.(*NoneType); return ok }

// AnyType is a runtime-tagged value box. Any value (int, float, bool, str,
// bytes, list, dict, None) can be assigned to Any; the conversion boxes the
// value into a SpyAny tagged union at the LLVM level (see runtime.c). The
// reverse direction requires explicit unboxing via the any_int / any_str /
// any_float / any_bool / any_list / any_dict / any_bytes builtins, which
// raise TypeError when the runtime tag doesn't match the requested type.
//
// Any exists primarily so JSON values (whose shape isn't statically known)
// and other heterogeneous payloads (request/response bodies, dynamic
// configuration) can be expressed in spython's otherwise-static type system.
type AnyType struct{}

func (t *AnyType) String() string    { return "Any" }
func (t *AnyType) Equals(o Type) bool { _, ok := o.(*AnyType); return ok }

// IteratorType is the static type of a generator function's return value.
// Today only generator functions (def with `yield` body, declared
// `-> Iterator[T]`) produce these; user-defined iterator classes are not
// yet covered. Iteration consumers (`for`, `next()`) treat any
// IteratorType as opaque and dispatch through the synthesized
// __iter__/__next__ methods.
type IteratorType struct {
	Elem Type
}

func (t *IteratorType) String() string { return fmt.Sprintf("Iterator[%s]", t.Elem.String()) }
func (t *IteratorType) Equals(o Type) bool {
	ot, ok := o.(*IteratorType)
	if !ok {
		return false
	}
	return t.Elem.Equals(ot.Elem)
}

type ListType struct {
	Elem Type
}

func (t *ListType) String() string { return fmt.Sprintf("list[%s]", t.Elem.String()) }
func (t *ListType) Equals(o Type) bool {
	ot, ok := o.(*ListType)
	if !ok {
		return false
	}
	return t.Elem.Equals(ot.Elem)
}

type TupleType struct {
	Elements []Type
}

func (t *TupleType) String() string {
	s := "tuple["
	for i, e := range t.Elements {
		if i > 0 {
			s += ", "
		}
		s += e.String()
	}
	return s + "]"
}

func (t *TupleType) Equals(o Type) bool {
	ot, ok := o.(*TupleType)
	if !ok {
		return false
	}
	if len(t.Elements) != len(ot.Elements) {
		return false
	}
	for i := range t.Elements {
		if !t.Elements[i].Equals(ot.Elements[i]) {
			return false
		}
	}
	return true
}

type SetType struct {
	Elem Type
}

func (t *SetType) String() string { return fmt.Sprintf("set[%s]", t.Elem.String()) }
func (t *SetType) Equals(o Type) bool {
	ot, ok := o.(*SetType)
	if !ok {
		return false
	}
	return t.Elem.Equals(ot.Elem)
}

type MapType struct {
	Key   Type
	Value Type
}

func (t *MapType) String() string {
	return fmt.Sprintf("map[%s, %s]", t.Key.String(), t.Value.String())
}

func (t *MapType) Equals(o Type) bool {
	ot, ok := o.(*MapType)
	if !ok {
		return false
	}
	return t.Key.Equals(ot.Key) && t.Value.Equals(ot.Value)
}

type FuncType struct {
	Params     []Type
	ParamNames []string // names matching Params, used for kwarg binding
	// KwOnlyStart is the index in Params at which keyword-only parameters
	// begin (those declared after *args). Equal to len(Params) when no
	// keyword-only params exist.
	KwOnlyStart int
	// VarArgsElem is the element type of *args, or nil when the function
	// has no *args parameter. The varargs name is in VarArgsName.
	VarArgsElem Type
	VarArgsName string
	// KwargsElem is the value type of **kwargs (keys are always str), or
	// nil when the function has no **kwargs parameter.
	KwargsElem Type
	KwargsName string
	Return     Type
	DefinedIn  string // module ID where this function is defined; "" for anonymous/method types
	// ExternSymbol, when non-empty, overrides the default call-site mangling.
	// Set by @extern("name") declarations; code generation uses this literal
	// C symbol instead of spy_<module>_<name>.
	ExternSymbol string
	// ParamDefaults is parallel to Params: ParamDefaults[i] is the default
	// expression for Params[i], or nil when the parameter is required.
	// Defaults are inlined at each call site that omits the slot (the
	// expression is re-emitted per call, not shared via a global).
	ParamDefaults []parser.Expr
	// Closure marks a first-class callable value (a lambda, nested def, or a
	// Callable[...] annotation) as opposed to a statically-dispatched named
	// function symbol. Closure values are represented at runtime as an i8*
	// to a heap environment whose first slot is the function pointer.
	Closure bool
}

func (t *FuncType) String() string {
	parts := []string{}
	kwStart := t.KwOnlyStart
	if kwStart == 0 && len(t.Params) > 0 && t.VarArgsElem == nil {
		kwStart = len(t.Params)
	}
	for i, p := range t.Params {
		if i == kwStart && t.VarArgsElem != nil {
			parts = append(parts, "*"+t.VarArgsElem.String())
		}
		parts = append(parts, p.String())
	}
	if t.VarArgsElem != nil && kwStart == len(t.Params) {
		parts = append(parts, "*"+t.VarArgsElem.String())
	}
	if t.KwargsElem != nil {
		parts = append(parts, "**"+t.KwargsElem.String())
	}
	return fmt.Sprintf("(%s) -> %s", strings.Join(parts, ", "), t.Return.String())
}

func (t *FuncType) Equals(o Type) bool {
	ot, ok := o.(*FuncType)
	if !ok {
		return false
	}
	if len(t.Params) != len(ot.Params) {
		return false
	}
	for i := range t.Params {
		if !t.Params[i].Equals(ot.Params[i]) {
			return false
		}
	}
	if t.KwOnlyStart != ot.KwOnlyStart {
		return false
	}
	if (t.VarArgsElem == nil) != (ot.VarArgsElem == nil) {
		return false
	}
	if t.VarArgsElem != nil && !t.VarArgsElem.Equals(ot.VarArgsElem) {
		return false
	}
	if (t.KwargsElem == nil) != (ot.KwargsElem == nil) {
		return false
	}
	if t.KwargsElem != nil && !t.KwargsElem.Equals(ot.KwargsElem) {
		return false
	}
	return t.Return.Equals(ot.Return)
}

type ModuleType struct {
	ID      string
	Exports map[string]Type
}

func (t *ModuleType) String() string { return fmt.Sprintf("module(%s)", t.ID) }
func (t *ModuleType) Equals(o Type) bool {
	ot, ok := o.(*ModuleType)
	if !ok {
		return false
	}
	return t.ID == ot.ID
}

// ClassField describes one instance field of a ClassType.
type ClassField struct {
	Name string
	Type Type
}

// ClassType is the type of the class itself (not instances).
// Calling it as a value (e.g., `Circle(1.0)`) produces an InstanceType.
type ClassType struct {
	Name       string
	Base       *ClassType              // nil for no base
	Fields     []ClassField            // ordered layout; inherited fields come first
	FieldIdx   map[string]int          // name -> Fields index
	Methods    map[string]*FuncType    // resolved method signatures (self-less)
	OwnMethods map[string]bool         // methods defined directly on this class
	MethodSrc  map[string]*ClassType   // method name -> defining class (for vtable / super)
	ClassID    int                     // unique non-zero id; used by isinstance
	DefinedIn  string                  // module ID where this class is defined
}

func (t *ClassType) String() string    { return fmt.Sprintf("class(%s)", t.Name) }
func (t *ClassType) Equals(o Type) bool {
	ot, ok := o.(*ClassType)
	if !ok {
		return false
	}
	return t == ot
}

// IsSubclassOf reports whether t is the same class as other or descends from it.
func (t *ClassType) IsSubclassOf(other *ClassType) bool {
	for c := t; c != nil; c = c.Base {
		if c == other {
			return true
		}
	}
	return false
}

// InstanceType is the type of an object created by calling a ClassType.
type InstanceType struct {
	Class *ClassType
}

func (t *InstanceType) String() string { return t.Class.Name }
func (t *InstanceType) Equals(o Type) bool {
	ot, ok := o.(*InstanceType)
	if !ok {
		return false
	}
	return t.Class == ot.Class
}

// IsAssignable reports whether a value of type `from` can be assigned to a
// variable/parameter of type `to`. It is identity for primitives and allows
// subclass -> superclass widening for instance types. Any boxes any other
// type, and container types (list/map) propagate Any through their element
// or value position so list[int] -> list[Any] and map[str,int] -> map[str,Any]
// both succeed (the boxing happens at the codegen layer when literals are
// emitted into an Any-shaped target).
func IsAssignable(from, to Type) bool {
	// A None literal is assignable to any instance type — lets users start
	// fields as None and reassign later. (Not currently used; reserved.)
	if fi, ok := from.(*InstanceType); ok {
		if ti, ok := to.(*InstanceType); ok {
			return fi.Class.IsSubclassOf(ti.Class)
		}
	}
	// Anything is assignable to Any (boxing).
	if _, ok := to.(*AnyType); ok {
		return true
	}
	// Containers propagate assignability through their element/value type
	// so map[K, V1] -> map[K, V2] when V1 -> V2 (mostly used for V2 = Any).
	if fl, ok := from.(*ListType); ok {
		if tl, ok := to.(*ListType); ok {
			return IsAssignable(fl.Elem, tl.Elem)
		}
	}
	if fm, ok := from.(*MapType); ok {
		if tm, ok := to.(*MapType); ok {
			return fm.Key.Equals(tm.Key) && IsAssignable(fm.Value, tm.Value)
		}
	}
	return from.Equals(to)
}
