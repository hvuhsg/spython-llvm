package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func projectRoot() string {
	// tests/ is one level below root
	dir, _ := os.Getwd()
	return filepath.Dir(dir)
}

func TestIntegration(t *testing.T) {
	root := projectRoot()

	// Build the compiler first
	compilerBin := filepath.Join(os.TempDir(), "spython-test-bin")
	defer os.Remove(compilerBin)

	buildCmd := exec.Command("go", "build", "-o", compilerBin, "./cmd/spython")
	buildCmd.Dir = root
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build compiler: %v\n%s", err, out)
	}

	// Find test data files
	testdataDir := filepath.Join(root, "testdata")
	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			// Directory fixture: expect main.spy + main.expected inside.
			mainSpy := filepath.Join(testdataDir, entry.Name(), "main.spy")
			expectedFile := filepath.Join(testdataDir, entry.Name(), "main.expected")
			if _, err := os.Stat(mainSpy); err != nil {
				continue
			}
			if _, err := os.Stat(expectedFile); err != nil {
				continue
			}
			t.Run(entry.Name(), func(t *testing.T) {
				expected, err := os.ReadFile(expectedFile)
				if err != nil {
					t.Fatal(err)
				}
				runCmd := exec.Command(compilerBin, "run", mainSpy)
				runCmd.Dir = root
				output, err := runCmd.CombinedOutput()
				if err != nil {
					t.Fatalf("run failed: %v\n%s", err, output)
				}
				got := string(output)
				want := string(expected)
				if got != want {
					t.Errorf("output mismatch for %s:\ngot:\n%s\nwant:\n%s", entry.Name(), got, want)
				}
			})
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".spy") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".spy")
		expectedFile := filepath.Join(testdataDir, name+".expected")

		if _, err := os.Stat(expectedFile); os.IsNotExist(err) {
			continue
		}

		t.Run(name, func(t *testing.T) {
			expected, err := os.ReadFile(expectedFile)
			if err != nil {
				t.Fatal(err)
			}

			spyFile := filepath.Join(testdataDir, entry.Name())
			runCmd := exec.Command(compilerBin, "run", spyFile)
			runCmd.Dir = root
			output, err := runCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, output)
			}

			got := string(output)
			want := string(expected)
			if got != want {
				t.Errorf("output mismatch for %s:\ngot:\n%s\nwant:\n%s", entry.Name(), got, want)
			}
		})
	}
}
