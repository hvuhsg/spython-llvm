package types

type Env struct {
	scopes  []map[string]Type
	classes map[string]*ClassType
}

func NewEnv() *Env {
	return &Env{
		scopes:  []map[string]Type{make(map[string]Type)},
		classes: make(map[string]*ClassType),
	}
}

func (e *Env) Push() {
	e.scopes = append(e.scopes, make(map[string]Type))
}

func (e *Env) Pop() {
	if len(e.scopes) > 1 {
		e.scopes = e.scopes[:len(e.scopes)-1]
	}
}

func (e *Env) Define(name string, t Type) {
	e.scopes[len(e.scopes)-1][name] = t
}

func (e *Env) Lookup(name string) (Type, bool) {
	for i := len(e.scopes) - 1; i >= 0; i-- {
		if t, ok := e.scopes[i][name]; ok {
			return t, true
		}
	}
	return nil, false
}

func (e *Env) DefineClass(name string, c *ClassType) {
	e.classes[name] = c
}

func (e *Env) LookupClass(name string) (*ClassType, bool) {
	c, ok := e.classes[name]
	return c, ok
}

// Classes returns the registered classes (name -> ClassType). Used by codegen
// to iterate all class definitions.
func (e *Env) Classes() map[string]*ClassType {
	return e.classes
}
