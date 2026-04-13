package loader

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestImportCycleDetected(t *testing.T) {
	entry, err := filepath.Abs("../testdata/errors/import_cycle/main.spy")
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(entry)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "import cycle") {
		t.Errorf("error should mention 'import cycle', got: %s", msg)
	}
	// Chain should include both modules that participate in the cycle.
	for _, want := range []string{"main.spy", "a.spy", "b.spy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %s, got: %s", want, msg)
		}
	}
	// a.spy must appear twice — once as imported from main, once re-entered from b.
	if strings.Count(msg, "a.spy") < 2 {
		t.Errorf("error should show a.spy twice (cycle re-entry), got: %s", msg)
	}
}

func TestImportCollisionDetected(t *testing.T) {
	entry, err := filepath.Abs("../testdata/errors/import_collision/main.spy")
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(entry)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "collision") {
		t.Errorf("error should mention 'collision', got: %s", msg)
	}
	if !strings.Contains(msg, `"bar"`) {
		t.Errorf("error should name the colliding symbol 'bar', got: %s", msg)
	}
	if !strings.Contains(msg, "foo") {
		t.Errorf("error should name the source module 'foo', got: %s", msg)
	}
}
