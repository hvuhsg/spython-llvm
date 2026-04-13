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

	// Type-check each module in topological order (entry last).
	for _, m := range l.order {
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
		depPath := filepath.Join(dir, depName+".spy")
		if _, err := os.Stat(depPath); err != nil {
			return nil, fmt.Errorf("%s: cannot resolve import %q: no %s.spy in %s", absPath, depName, depName, dir)
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

// validateImportedTopLevel rejects non-entry modules that contain anything at
// top level other than def, typed constant assignments, and import statements.
func validateImportedTopLevel(program *parser.Program, modID string) error {
	for _, stmt := range program.Stmts {
		switch s := stmt.(type) {
		case *parser.FuncDef, *parser.ImportStmt, *parser.FromImportStmt:
			// allowed
		case *parser.AssignStmt:
			if s.TypeAnn == nil {
				return fmt.Errorf("module %s: top-level reassignments not allowed in imported modules (line %d)", modID, s.Pos.Line)
			}
			if !isConstExpr(s.Value) {
				return fmt.Errorf("module %s: top-level assignment of %s must have a constant-literal value in imported modules (line %d)", modID, s.Name, s.Pos.Line)
			}
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
