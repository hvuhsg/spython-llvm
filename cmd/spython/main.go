package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yehoyadashtinmetz/spython/codegen"
	"github.com/yehoyadashtinmetz/spython/loader"
	"github.com/yehoyadashtinmetz/spython/parser"
	"github.com/yehoyadashtinmetz/spython/types"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: spython <build|run> <file.spy>")
		os.Exit(1)
	}

	cmd := os.Args[1]
	file := os.Args[2]

	var outputName string
	if cmd == "build" && len(os.Args) >= 5 && os.Args[2] == "-o" {
		outputName = os.Args[3]
		file = os.Args[4]
	}

	switch cmd {
	case "build":
		if outputName == "" {
			outputName = strings.TrimSuffix(filepath.Base(file), ".spy")
		}
		if err := build(file, outputName); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "run":
		tmpOutput, err := os.CreateTemp("", "spython-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		tmpName := tmpOutput.Name()
		tmpOutput.Close()
		defer os.Remove(tmpName)

		if err := build(file, tmpName); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		runCmd := exec.Command(tmpName)
		runCmd.Stdout = os.Stdout
		runCmd.Stderr = os.Stderr
		if err := runCmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(1)
	}
}

func build(file, output string) error {
	// Load + type-check all modules (entry + transitive imports)
	res, err := loader.Load(file)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	// Codegen: convert loader Modules to codegen.ModuleInput
	var mods []*codegen.ModuleInput
	var entry *codegen.ModuleInput
	for _, m := range res.Modules {
		var classes []*types.ClassType
		for _, stmt := range m.Program.Stmts {
			if cd, ok := stmt.(*parser.ClassDef); ok {
				if ct, ok := m.Checker.Env.LookupClass(cd.Name); ok {
					classes = append(classes, ct)
				}
			}
		}
		mi := &codegen.ModuleInput{
			ID:      m.ID,
			Program: m.Program,
			Deps:    m.Deps,
			IsEntry: m.IsEntry,
			Classes: classes,
		}
		mods = append(mods, mi)
		if m.IsEntry {
			entry = mi
		}
	}
	gen := codegen.New()
	ir, err := gen.GenerateAll(mods, entry)
	if err != nil {
		return fmt.Errorf("codegen: %w", err)
	}

	// Write IR to temp file
	tmpIR, err := os.CreateTemp("", "spython-*.ll")
	if err != nil {
		return err
	}
	defer os.Remove(tmpIR.Name())

	if _, err := tmpIR.WriteString(ir); err != nil {
		tmpIR.Close()
		return err
	}
	tmpIR.Close()

	// Find runtime.c
	runtimePath := findRuntime()

	// Collect C-backed stdlib modules and their link-directive flags.
	var cFiles []string
	var linkFlags []string
	for _, m := range res.Modules {
		if m.CFile != "" {
			cFiles = append(cFiles, m.CFile)
			linkFlags = append(linkFlags, m.LinkFlags...)
		}
	}
	linkFlags = loader.DedupeFlags(linkFlags)

	// Compile with clang
	args := []string{"-O2", "-o", output, tmpIR.Name(), runtimePath}
	args = append(args, cFiles...)
	runtimeDir := filepath.Dir(runtimePath)
	args = append(args, "-I"+runtimeDir)
	if runtime.GOOS == "darwin" {
		args = append(args, "-Wno-override-module")
		for _, prefix := range []string{"/opt/homebrew/opt/bdw-gc", "/usr/local/opt/bdw-gc"} {
			if _, err := os.Stat(filepath.Join(prefix, "include", "gc.h")); err == nil {
				args = append(args, "-I"+filepath.Join(prefix, "include"), "-L"+filepath.Join(prefix, "lib"))
				break
			}
		}
	}
	args = append(args, "-lgc")
	args = append(args, linkFlags...)

	clangCmd := exec.Command("clang", args...)
	clangCmd.Stderr = os.Stderr
	if err := clangCmd.Run(); err != nil {
		return fmt.Errorf("clang: %w", err)
	}

	return nil
}

func findRuntime() string {
	// Try relative to executable
	exePath, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exePath)
		candidate := filepath.Join(dir, "..", "runtime", "runtime.c")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		candidate = filepath.Join(dir, "runtime", "runtime.c")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Try relative to working directory
	candidates := []string{
		"runtime/runtime.c",
		"../runtime/runtime.c",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}

	return "runtime/runtime.c"
}
