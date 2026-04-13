package codegen

import "testing"

func TestNewGenerator(t *testing.T) {
	g := New()
	_, err := g.Generate(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
