package types

import "testing"

func TestNewChecker(t *testing.T) {
	c := NewChecker()
	err := c.Check(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
