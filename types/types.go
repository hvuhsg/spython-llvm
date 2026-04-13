package types

import "fmt"

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

type NoneType struct{}

func (t *NoneType) String() string    { return "None" }
func (t *NoneType) Equals(o Type) bool { _, ok := o.(*NoneType); return ok }

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
	Params    []Type
	Return    Type
	DefinedIn string // module ID where this function is defined; "" for anonymous/method types
}

func (t *FuncType) String() string {
	params := ""
	for i, p := range t.Params {
		if i > 0 {
			params += ", "
		}
		params += p.String()
	}
	return fmt.Sprintf("(%s) -> %s", params, t.Return.String())
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
// subclass -> superclass widening for instance types.
func IsAssignable(from, to Type) bool {
	// A None literal is assignable to any instance type — lets users start
	// fields as None and reassign later. (Not currently used; reserved.)
	if fi, ok := from.(*InstanceType); ok {
		if ti, ok := to.(*InstanceType); ok {
			return fi.Class.IsSubclassOf(ti.Class)
		}
	}
	return from.Equals(to)
}
