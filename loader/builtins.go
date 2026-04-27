package loader

import (
	"fmt"

	"github.com/yehoyadashtinmetz/spython/lexer"
	"github.com/yehoyadashtinmetz/spython/parser"
	"github.com/yehoyadashtinmetz/spython/types"
)

// builtinsModuleID is the synthetic module that owns the auto-injected
// exception hierarchy. It has no on-disk source; its Program is parsed from
// builtinExceptionsSource. Mangled symbols use this as the module prefix
// (e.g. @spy_builtins_Exception___init__).
const builtinsModuleID = "builtins"

// builtinExceptionsSource is parsed into AST nodes that become the body of
// the synthetic `builtins` module. Every user module's checker has the
// resulting ClassType pointers injected into its env via
// Checker.InjectBuiltins, so `raise X` / `except T` work from any module
// without each module creating its own (incompatible) Exception hierarchy.
//
// Matches CPython's exception tree where it matters (ArithmeticError wraps
// ZeroDivisionError; LookupError wraps IndexError/KeyError; everything
// derives from Exception). Each class carries a single `msg: str` field.
const builtinExceptionsSource = `class Exception:
    def __init__(self, msg: str):
        self.msg = msg

class ArithmeticError(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class ZeroDivisionError(ArithmeticError):
    def __init__(self, msg: str):
        super().__init__(msg)

class LookupError(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class IndexError(LookupError):
    def __init__(self, msg: str):
        super().__init__(msg)

class KeyError(LookupError):
    def __init__(self, msg: str):
        super().__init__(msg)

class StopIteration(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class ValueError(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class TypeError(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class RuntimeError(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class NameError(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class AttributeError(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class OSError(Exception):
    def __init__(self, msg: str):
        super().__init__(msg)

class FileNotFoundError(OSError):
    def __init__(self, msg: str):
        super().__init__(msg)

class PermissionError(OSError):
    def __init__(self, msg: str):
        super().__init__(msg)

class IsADirectoryError(OSError):
    def __init__(self, msg: str):
        super().__init__(msg)

class ConnectionRefusedError(OSError):
    def __init__(self, msg: str):
        super().__init__(msg)
`

// loadBuiltinsModule parses builtinExceptionsSource and returns a synthetic
// Module ready to be type-checked first in the topological order. The
// resulting ClassType pointers are then shared across every other module's
// checker via Checker.InjectBuiltins.
func loadBuiltinsModule() (*Module, error) {
	lx := lexer.New(builtinExceptionsSource)
	toks, err := lx.Tokens()
	if err != nil {
		return nil, fmt.Errorf("builtins: lexer: %w", err)
	}
	p := parser.New(toks)
	p.SetFile("<builtins>")
	prog, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("builtins: parser: %w", err)
	}
	return &Module{
		ID:      builtinsModuleID,
		Path:    "<builtins>",
		Program: prog,
	}, nil
}

// checkBuiltinsModule type-checks the synthetic builtins module and returns
// the resulting checker. The caller uses checker.Env.Classes() to obtain the
// canonical class pointers to inject into every other module. classIDSrc is
// the shared class-ID counter; passing the same pointer to every module's
// checker keeps class IDs unique program-wide so isinstance comparisons
// don't false-match across modules.
func checkBuiltinsModule(m *Module, classIDSrc *int) (*types.Checker, error) {
	checker := types.NewCheckerWithImports(m.ID, nil)
	checker.SetClassIDSource(classIDSrc)
	if err := checker.Check(m.Program); err != nil {
		return nil, fmt.Errorf("builtins: %w", err)
	}
	return checker, nil
}
