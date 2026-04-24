package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yehoyadashtinmetz/spython/lexer"
	"github.com/yehoyadashtinmetz/spython/parser"
	"github.com/yehoyadashtinmetz/spython/types"
)

type Module struct {
	ID      string
	Path    string
	Program *parser.Program
	Checker *types.Checker
	Deps    []string
	IsEntry bool
	// CFile is the path to the sibling C implementation for this module, if any.
	// Populated for modules that contain one or more @extern function
	// declarations and where <module>.c exists next to <module>.spy.
	CFile string
	// LinkFlags holds flags parsed from `// spython-link:` directives in CFile.
	LinkFlags []string
}

type Result struct {
	Modules []*Module
	Entry   *Module
}

type loader struct {
	cache map[string]*Module // canonical path -> loaded module (after parse)
	order []*Module          // topological, entry last
	grey  map[string]bool    // cycle-detection: currently on DFS stack
	stack []string           // for cycle error messages
}

func Load(entryFile string) (*Result, error) {
	abs, err := filepath.Abs(entryFile)
	if err != nil {
		return nil, fmt.Errorf("resolve entry path: %w", err)
	}

	l := &loader{
		cache: map[string]*Module{},
		grey:  map[string]bool{},
	}

	entry, err := l.loadPath(abs, true)
	if err != nil {
		return nil, err
	}

	// Synthetic builtins module owns the auto-injected exception hierarchy.
	// It is type-checked first to produce a single, canonical set of
	// ClassType pointers; every other module then has those pointers
	// injected into its env so `raise X` / `except T` work everywhere
	// (including stdlib modules), and so isinstance checks via pointer
	// identity continue to hold across module boundaries.
	builtins, err := loadBuiltinsModule()
	if err != nil {
		return nil, err
	}
	// Shared class-ID counter: every checker in this Load increments the
	// same counter so each ClassType.ClassID is unique program-wide. The
	// runtime's isinstance walks vtable->class_id chains and a duplicate
	// would silently false-match — see the try_rethrow regression that
	// prompted this.
	var classIDCtr int
	builtinsChecker, err := checkBuiltinsModule(builtins, &classIDCtr)
	if err != nil {
		return nil, err
	}
	builtins.Checker = builtinsChecker
	builtinClasses := builtinsChecker.Env.Classes()
	// Builtins comes first so codegen registers its classes (and their
	// vtables) before any user class that might inherit from them.
	l.order = append([]*Module{builtins}, l.order...)

	// Type-check user modules in topological order (entry last).
	for _, m := range l.order {
		if m == builtins {
			continue
		}
		imports := map[string]*types.ModuleType{}
		for _, dep := range m.Deps {
			depMod := l.findByID(dep)
			if depMod == nil {
				return nil, fmt.Errorf("internal: dep module %q not found", dep)
			}
			imports[dep] = &types.ModuleType{
				ID:      depMod.ID,
				Exports: depMod.Checker.Exports(depMod.Program),
			}
		}
		checker := types.NewCheckerWithImports(m.ID, imports)
		checker.SetClassIDSource(&classIDCtr)
		checker.InjectBuiltins(builtinClasses)
		if err := checker.Check(m.Program); err != nil {
			return nil, fmt.Errorf("%s: %w", m.Path, err)
		}
		m.Checker = checker
	}

	return &Result{Modules: l.order, Entry: entry}, nil
}

func (l *loader) findByID(id string) *Module {
	for _, m := range l.order {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (l *loader) loadPath(absPath string, isEntry bool) (*Module, error) {
	if l.grey[absPath] {
		chain := strings.Join(append(append([]string{}, l.stack...), absPath), " -> ")
		return nil, fmt.Errorf("import cycle: %s", chain)
	}
	if existing, ok := l.cache[absPath]; ok {
		return existing, nil
	}

	source, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", absPath, err)
	}

	lx := lexer.New(string(source))
	tokens, err := lx.Tokens()
	if err != nil {
		return nil, fmt.Errorf("%s: lexer: %w", absPath, err)
	}

	p := parser.New(tokens)
	p.SetFile(absPath)
	program, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("%s: parser: %w", absPath, err)
	}

	id := moduleIDFromPath(absPath)
	m := &Module{
		ID:      id,
		Path:    absPath,
		Program: program,
		IsEntry: isEntry,
	}

	if !isEntry {
		if err := validateImportedTopLevel(program, id); err != nil {
			return nil, err
		}
	}

	// If this module has any @extern declarations that rely on default name
	// mangling (spy_<module>_<name>), look for a sibling C implementation file
	// that provides those symbols. Modules whose externs all specify an
	// explicit ExternSymbol are binding to symbols linked from elsewhere and
	// do not require a sibling .c.
	if needsSiblingC(program) {
		cPath := strings.TrimSuffix(absPath, ".spy") + ".c"
		if _, err := os.Stat(cPath); err != nil {
			return nil, fmt.Errorf("%s: module %q has @extern declarations with default mangling but sibling %s not found",
				absPath, id, filepath.Base(cPath))
		}
		flags, err := ParseLinkDirectives(cPath)
		if err != nil {
			return nil, err
		}
		m.CFile = cPath
		m.LinkFlags = flags
	} else if hasExternDecls(program) {
		// Still attach a sibling .c if one happens to exist — lets users opt in
		// to providing extra symbols alongside a file that uses only explicit
		// ExternSymbols.
		cPath := strings.TrimSuffix(absPath, ".spy") + ".c"
		if _, err := os.Stat(cPath); err == nil {
			flags, err := ParseLinkDirectives(cPath)
			if err != nil {
				return nil, err
			}
			m.CFile = cPath
			m.LinkFlags = flags
		}
	}

	l.cache[absPath] = m
	l.grey[absPath] = true
	l.stack = append(l.stack, absPath)

	// Recurse into deps. Use a set so the same module imported twice only adds
	// one Deps entry.
	depSet := map[string]bool{}
	dir := filepath.Dir(absPath)
	for _, stmt := range program.Stmts {
		var depName string
		switch s := stmt.(type) {
		case *parser.ImportStmt:
			depName = s.Module
		case *parser.FromImportStmt:
			depName = s.Module
		default:
			continue
		}
		depPath, err := resolveImport(dir, depName)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot resolve import %q: %w", absPath, depName, err)
		}
		depAbs, err := filepath.Abs(depPath)
		if err != nil {
			return nil, err
		}
		if _, err := l.loadPath(depAbs, false); err != nil {
			return nil, err
		}
		depID := moduleIDFromPath(depAbs)
		if !depSet[depID] {
			depSet[depID] = true
			m.Deps = append(m.Deps, depID)
		}
	}

	l.stack = l.stack[:len(l.stack)-1]
	delete(l.grey, absPath)

	l.order = append(l.order, m)
	return m, nil
}

func moduleIDFromPath(absPath string) string {
	return strings.TrimSuffix(filepath.Base(absPath), ".spy")
}

// resolveImport looks for <name>.spy first in dir, then in the stdlib search
// paths. Returns the absolute path of the first match.
func resolveImport(dir, name string) (string, error) {
	local := filepath.Join(dir, name+".spy")
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}
	for _, root := range stdlibSearchPaths() {
		p := filepath.Join(root, name+".spy")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no %s.spy in %s or stdlib", name, dir)
}

// stdlibSearchPaths returns directories to search for stdlib modules, in
// priority order: SPYTHON_HOME/stdlib, then directories relative to the
// spython executable, then a development-tree fallback.
func stdlibSearchPaths() []string {
	var paths []string
	if home := os.Getenv("SPYTHON_HOME"); home != "" {
		paths = append(paths, filepath.Join(home, "stdlib"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		paths = append(paths,
			filepath.Join(dir, "stdlib"),
			filepath.Join(dir, "..", "stdlib"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths,
			filepath.Join(wd, "stdlib"),
			filepath.Join(wd, "..", "stdlib"),
		)
	}
	return paths
}

// hasExternDecls reports whether program contains any @extern function
// declarations at top level.
func hasExternDecls(program *parser.Program) bool {
	if program == nil {
		return false
	}
	for _, stmt := range program.Stmts {
		if fd, ok := stmt.(*parser.FuncDef); ok && fd.Extern {
			return true
		}
	}
	return false
}

// needsSiblingC reports whether the program contains at least one @extern
// declaration that relies on default name mangling — i.e. one without an
// explicit ExternSymbol. Such declarations expect their symbol to be provided
// by the sibling <module>.c file.
func needsSiblingC(program *parser.Program) bool {
	if program == nil {
		return false
	}
	for _, stmt := range program.Stmts {
		if fd, ok := stmt.(*parser.FuncDef); ok && fd.Extern && fd.ExternSymbol == "" {
			return true
		}
	}
	return false
}

// validateImportedTopLevel rejects non-entry modules that contain anything at
// top level other than def, class, constant assignments, and import statements.
// Constants may omit the type annotation (the checker infers it from the RHS)
// but each name may only be bound once — rebinding would be a reassignment,
// which is not allowed in imported modules.
func validateImportedTopLevel(program *parser.Program, modID string) error {
	seen := make(map[string]bool)
	for _, stmt := range program.Stmts {
		switch s := stmt.(type) {
		case *parser.FuncDef, *parser.ImportStmt, *parser.FromImportStmt, *parser.ClassDef:
			// allowed
		case *parser.AssignStmt:
			if !isConstExpr(s.Value) {
				return fmt.Errorf("module %s: top-level assignment of %s must have a constant-literal value in imported modules (line %d)", modID, s.Name, s.Pos.Line)
			}
			if seen[s.Name] {
				return fmt.Errorf("module %s: top-level reassignments not allowed in imported modules (line %d)", modID, s.Pos.Line)
			}
			seen[s.Name] = true
		default:
			return fmt.Errorf("module %s: only def and typed constant assignments are allowed at top level (got %T at line %d)", modID, stmt, s.GetPos().Line)
		}
	}
	return nil
}

// isConstExpr returns true when expr is a literal (int/float/str/bool/None).
// v1 keeps this strict; arithmetic on literals can be added later.
func isConstExpr(expr parser.Expr) bool {
	switch expr.(type) {
	case *parser.IntLit, *parser.FloatLit, *parser.StrLit, *parser.BoolLit, *parser.NoneLit:
		return true
	}
	return false
}
