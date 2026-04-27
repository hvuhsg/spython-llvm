package main

import (
	"flag"
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

// buildOptions controls how the entry .spy file is compiled into a native
// binary. Empty fields fall back to host-native defaults.
type buildOptions struct {
	target     string   // clang -target triple (e.g. "aarch64-linux-gnu")
	sysroot    string   // --sysroot=<path>
	clangFlags []string // additional flags forwarded verbatim to clang
}

// stringSliceFlag implements flag.Value for repeatable string flags.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, " ") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

func usage() {
	fmt.Fprintln(os.Stderr, "usage: spython <build|run> [flags] <file.spy>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  build    compile <file.spy> to a native executable")
	fmt.Fprintln(os.Stderr, "  run      compile and immediately execute <file.spy>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "common flags:")
	fmt.Fprintln(os.Stderr, "  -target <triple>   clang target triple (e.g. aarch64-linux-gnu)")
	fmt.Fprintln(os.Stderr, "  -sysroot <path>    --sysroot for cross compilation")
	fmt.Fprintln(os.Stderr, "  -cflag <flag>      forward an extra flag to clang (repeatable)")
	fmt.Fprintln(os.Stderr, "build-only flags:")
	fmt.Fprintln(os.Stderr, "  -o <name>          output binary name")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var (
		outputName string
		opts       buildOptions
		extraFlags stringSliceFlag
	)

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	if cmd == "build" {
		fs.StringVar(&outputName, "o", "", "output binary name")
	}
	fs.StringVar(&opts.target, "target", "", "clang target triple")
	fs.StringVar(&opts.sysroot, "sysroot", "", "sysroot for cross compilation")
	fs.Var(&extraFlags, "cflag", "additional flag forwarded to clang (repeatable)")
	fs.Usage = usage

	switch cmd {
	case "build", "run":
		if err := fs.Parse(args); err != nil {
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(1)
	}

	opts.clangFlags = extraFlags

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "error: missing input file")
		usage()
		os.Exit(1)
	}
	file := fs.Arg(0)

	switch cmd {
	case "build":
		if outputName == "" {
			outputName = strings.TrimSuffix(filepath.Base(file), ".spy")
		}
		if err := build(file, outputName, opts); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "run":
		if opts.target != "" {
			fmt.Fprintln(os.Stderr, "warning: -target with `run` produces a binary for the target arch and will likely fail to execute on the host")
		}
		tmpOutput, err := os.CreateTemp("", "spython-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		tmpName := tmpOutput.Name()
		tmpOutput.Close()
		defer os.Remove(tmpName)

		if err := build(file, tmpName, opts); err != nil {
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
	}
}

func build(file, output string, opts buildOptions) error {
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
	// `-lm` is required by the runtime (float formatting uses log10/floor/fabs).
	// Stdlib modules like math.spy may also request it; DedupeFlags collapses
	// the duplicate.
	var cFiles []string
	linkFlags := []string{"-lm"}
	for _, m := range res.Modules {
		if m.CFile != "" {
			cFiles = append(cFiles, m.CFile)
			linkFlags = append(linkFlags, m.LinkFlags...)
		}
	}
	linkFlags = loader.DedupeFlags(linkFlags)

	// Build clang invocation. -Wno-override-module is always passed because the
	// IR carries a fixed target triple that clang will (legitimately) override
	// whenever the host or -target triple differs.
	args := []string{"-O2", "-o", output, tmpIR.Name(), runtimePath}
	args = append(args, cFiles...)
	runtimeDir := filepath.Dir(runtimePath)
	args = append(args, "-I"+runtimeDir, "-Wno-override-module")

	if opts.target != "" {
		args = append(args, "-target", opts.target)
	}
	if opts.sysroot != "" {
		args = append(args, "--sysroot="+opts.sysroot)
	}

	// Only consult the host's homebrew bdwgc when targeting the host; for a
	// cross build the user must supply the target's gc via -sysroot / -cflag.
	if opts.target == "" && runtime.GOOS == "darwin" {
		for _, prefix := range []string{"/opt/homebrew/opt/bdw-gc", "/usr/local/opt/bdw-gc"} {
			if _, err := os.Stat(filepath.Join(prefix, "include", "gc.h")); err == nil {
				args = append(args, "-I"+filepath.Join(prefix, "include"), "-L"+filepath.Join(prefix, "lib"))
				break
			}
		}
	}
	args = append(args, "-lgc")
	args = append(args, linkFlags...)
	args = append(args, opts.clangFlags...)

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
