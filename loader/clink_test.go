package loader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempC(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.c")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestParseLinkDirectives_SingleFlag(t *testing.T) {
	p := writeTempC(t, "// spython-link: -lm\nint x = 0;\n")
	flags, err := ParseLinkDirectives(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(flags) != 1 || flags[0] != "-lm" {
		t.Fatalf("flags = %v, want [-lm]", flags)
	}
}

func TestParseLinkDirectives_MultipleFlagsOneLine(t *testing.T) {
	p := writeTempC(t, "// spython-link: -lm -lpthread\n")
	flags, err := ParseLinkDirectives(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"-lm", "-lpthread"}
	if len(flags) != 2 || flags[0] != want[0] || flags[1] != want[1] {
		t.Fatalf("flags = %v, want %v", flags, want)
	}
}

func TestParseLinkDirectives_MultipleLines(t *testing.T) {
	body := "// spython-link: -lm\n// spython-link: -L/opt/lib -lcrypto\n"
	p := writeTempC(t, body)
	flags, err := ParseLinkDirectives(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"-lm", "-L/opt/lib", "-lcrypto"}
	if len(flags) != len(want) {
		t.Fatalf("flags = %v, want %v", flags, want)
	}
	for i := range want {
		if flags[i] != want[i] {
			t.Fatalf("flags[%d] = %q, want %q", i, flags[i], want[i])
		}
	}
}

func TestParseLinkDirectives_NoDirective(t *testing.T) {
	p := writeTempC(t, "int x = 0;\n")
	flags, err := ParseLinkDirectives(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(flags) != 0 {
		t.Fatalf("flags = %v, want empty", flags)
	}
}

func TestParseLinkDirectives_RejectsDangerousTokens(t *testing.T) {
	cases := []string{
		"-Wl,-rpath,/tmp",
		"-Wall",
		"; rm -rf /",
		"$(whoami)",
		"`id`",
		"-o/etc/passwd",
		"--shared",
	}
	for _, tok := range cases {
		t.Run(tok, func(t *testing.T) {
			p := writeTempC(t, "// spython-link: "+tok+"\n")
			_, err := ParseLinkDirectives(p)
			if err == nil {
				t.Fatalf("expected error for token %q", tok)
			}
			if !strings.Contains(err.Error(), "invalid link token") {
				t.Fatalf("error = %v, want 'invalid link token' substring", err)
			}
		})
	}
}

func TestParseLinkDirectives_EmptyDirectiveIsError(t *testing.T) {
	p := writeTempC(t, "// spython-link:   \n")
	_, err := ParseLinkDirectives(p)
	if err == nil {
		t.Fatal("expected error for empty directive")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error = %v, want 'empty' substring", err)
	}
}

func TestParseLinkDirectives_IgnoresDirectivesBelowScanLimit(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxLinkScanLines+5; i++ {
		sb.WriteString("// filler\n")
	}
	sb.WriteString("// spython-link: -lm\n")
	p := writeTempC(t, sb.String())
	flags, err := ParseLinkDirectives(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(flags) != 0 {
		t.Fatalf("flags = %v, want empty (directive below scan limit)", flags)
	}
}

func TestDedupeFlags(t *testing.T) {
	in := []string{"-lm", "-lpthread", "-lm", "-lcrypto", "-lpthread"}
	out := DedupeFlags(in)
	want := []string{"-lm", "-lpthread", "-lcrypto"}
	if len(out) != len(want) {
		t.Fatalf("out = %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out[%d] = %q, want %q", i, out[i], want[i])
		}
	}
}
