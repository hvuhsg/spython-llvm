# spython

A statically-typed subset of Python compiled to LLVM IR. Written in Go.

spython takes a strict, typed subset of Python, runs it through a clean
multi-pass frontend, emits LLVM IR, and hands off to `clang` to produce a
native binary. Heap memory is managed by the Boehm–Demers–Weiser conservative
garbage collector (bdwgc).

The goal is a practical, readable compiler that's small enough to reason about
end-to-end — not a full CPython replacement. Types are inferred from the RHS
at first binding (annotations only required when the RHS is ambiguous, e.g.
`None` or an empty container), dynamic features are intentionally absent, and
the runtime is a single C file.

## Pipeline

```
lexer → parser → loader → type checker → codegen → clang
```

Each stage is isolated in its own Go package.

## Example

```python
class Shape:
    def __init__(self):
        self.name = "shape"
    def area(self) -> float:
        return 0.0

class Circle(Shape):
    def __init__(self, r: float):
        super().__init__()
        self.name = "circle"
        self.r = r
    def area(self) -> float:
        return 3.14 * self.r * self.r

class Rect(Shape):
    def __init__(self, w: float, h: float):
        super().__init__()
        self.name = "rect"
        self.w = w
        self.h = h
    def area(self) -> float:
        return self.w * self.h

shapes: list[Shape] = [Circle(2.0), Rect(3.0, 4.0), Circle(1.0)]
total: float = 0.0
for s in shapes:
    total = total + s.area()
print(total)  # 27.7
```

## Getting started

### Prerequisites

```sh
# macOS
brew install go llvm bdw-gc

# Debian / Ubuntu
apt install golang clang libgc-dev
```

### Build

```sh
git clone https://github.com/yehoyadashtinmetz/spython
cd spython
go build -o spython ./cmd/spython
```

### Run

```sh
./spython run testdata/fizzbuzz.spy
./spython build -o myprog testdata/class_polymorphism/main.spy
./myprog
```

## What's supported

- **Types:** `int`, `float`, `bool`, `str`, `list[T]`, `dict[K, V]`, `set[T]`, `tuple`, `bytes`, `bytearray`, `None`, user-defined classes
- **Type inference:** local variable types are inferred from the RHS at first binding (`x = 1`, `xs = [1.0, 2.0]`); explicit annotations (`x: int = 1`) are only needed when the RHS is ambiguous (e.g. `xs: list[int] = []`, `parent: Node | None = None`)
- **Control flow:** `if / elif / else`, `while`, `for … in range`, `for … in list`, `for … in set`, `for … in dict` (yields keys), `for … in iterator`, `break`, `continue`, `return`
- **Container methods:** `str` (`upper`/`lower`/`capitalize`/`strip`/`lstrip`/`rstrip`/`startswith`/`endswith`/`find`/`rfind`/`count`/`replace`/`split`/`join`/`zfill`/`isdigit`/`isalpha`/`isspace`/`isupper`/`islower`), `list` (`append`/`pop`/`insert`/`remove`/`index`/`count`/`reverse`/`clear`/`extend`/`sort`), `dict` (`keys`/`values`/`get`/`update`/`clear`), `set` (`add`/`discard`) — set membership is `x in s`
- **Membership:** `x in y` / `x not in y` for str (substring), list, set, and dict (keys)
- **Functions:** `def` with required type annotations, recursion, nested calls, early return, `*args`, `**kwargs`, keyword-only parameters, default arguments, keyword arguments and `*` / `**` unpacking at call sites
- **Closures:** `lambda` expressions and nested `def`s as first-class values, with by-value capture of enclosing variables; the `Callable[[ArgTypes], Ret]` type annotation; closures passed as arguments (`key=`-style callbacks), returned from functions, stored in variables/lists. Lambda parameter types are inferred from the expected `Callable` type in context.
- **Generators:** `def f() -> Iterator[T]` with `yield`, `yield from`, the `next()` builtin, and `StopIteration`
- **Classes:** single inheritance, virtual dispatch (vtables), `super()`, `isinstance()`, field inference from `__init__`, implicit upcasting
- **Dunder methods:** `__init__`, `__str__`, `__repr__`, `__eq__`, `__ne__`, `__lt__`, `__le__`, `__gt__`, `__ge__`, `__add__`, `__sub__`, `__mul__`, `__truediv__`, `__floordiv__`, `__mod__`, `__neg__`, `__pow__`
- **Exceptions:** `raise`, `try / except / finally`, exception subclassing, propagation across calls; built-in hierarchy (`Exception`, `ArithmeticError`, `ZeroDivisionError`, `ValueError`, `TypeError`, `OSError`, …) auto-injected as a synthetic `builtins` module
- **Imports:** `import module`, `import module as alias`, `from module import name`, multi-file projects
- **Stdlib:** `math`, `random`, `time`, `io`, `os`, `os.path`, `sys`, `hashlib`, `binascii`, `base64`, `struct`, `socket`, `itertools`, `keyword`, `errno`, `stat`, `colorsys`, `re`, `fnmatch`, `string`, `textwrap`, `secrets`, `shutil` — implemented as `.spy` shims over sibling C files (FFI via `// spython-link:` directives) for the C-backed ones, pure `.spy` for the rest
- **Operators:** arithmetic (`+ - * / // % **`), comparison (`== != < > <= >=`), logical (`and`, `or`, `not`), bitwise (`& | ^ ~ << >>`), augmented assign (`+=`, `-=`, …)
- **Runtime:** `print`, `range`, `len`, `int()`, `float()`, `str()`, `bool()`, `isinstance()`, `sys.argv`, conservative GC, list/str/map indexing

## What's not supported

- Multiple inheritance, MRO
- Decorators (`@property`, `@staticmethod`, `@classmethod`, …)
- `async` / `await`
- Dynamic typing — types are fixed at first binding (inferred from the RHS, or annotated when the RHS is ambiguous like `None` / empty containers); a name's type cannot change afterwards
- Comprehensions (list / dict / set / generator)
- Metaclasses, `__new__`, `__slots__`, descriptors
- Context managers (`with`)
- `eval`, `exec`, `getattr` / `setattr` / `hasattr`
- Monkey-patching — classes are closed after definition

## Repository layout

```
cmd/spython   CLI entry point (build / run)
lexer         Tokenizer
parser        AST + parser
loader        Module loading, dependency resolution, builtins injection, C-link discovery
types         Type checker and type environment
codegen       LLVM IR emitter (raw text, no bindings)
runtime       runtime.c — single C file linked into every binary
stdlib        .spy + .c sibling pairs for math, io, os, socket, hashlib, …
errors        Error formatting
testdata      End-to-end .spy programs with expected output
tests         Go test suites
docs          Landing page (GitHub Pages)
```

## Tests

```sh
go test ./...
```

Integration tests compile every `testdata/*.spy` program and diff its output
against the matching `.expected` file.
