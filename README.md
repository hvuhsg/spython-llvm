# spython

A statically-typed subset of Python compiled to LLVM IR. Written in Go.

spython takes a strict, typed subset of Python, runs it through a clean
multi-pass frontend, emits LLVM IR, and hands off to `clang` to produce a
native binary. Heap memory is managed by the Boehm–Demers–Weiser conservative
garbage collector (bdwgc).

The goal is a practical, readable compiler that's small enough to reason about
end-to-end — not a full CPython replacement. Type annotations are required on
first binding, dynamic features are intentionally absent, and the runtime is
a single C file.

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

- **Types:** `int`, `float`, `bool`, `str`, `list[T]`, `dict[K, V]`, `None`, user-defined classes
- **Control flow:** `if / elif / else`, `while`, `for … in range`, `for … in list`, `break`, `continue`, `return`
- **Functions:** `def` with required type annotations, recursion, nested calls, early return
- **Classes:** single inheritance, virtual dispatch (vtables), `super()`, `isinstance()`, field inference from `__init__`, implicit upcasting
- **Dunder methods:** `__init__`, `__str__`, `__repr__`, `__eq__`, `__ne__`, `__lt__`, `__le__`, `__gt__`, `__ge__`, `__add__`, `__sub__`, `__mul__`, `__truediv__`, `__floordiv__`, `__mod__`, `__neg__`, `__pow__`
- **Imports:** `import module`, `import module as alias`, `from module import name`, multi-file projects
- **Operators:** arithmetic (`+ - * / // % **`), comparison (`== != < > <= >=`), logical (`and`, `or`, `not`), bitwise (`& | ^ ~ << >>`), augmented assign (`+=`, `-=`, …)
- **Runtime:** `print`, `range`, `len`, `int()`, `float()`, `str()`, `bool()`, `isinstance()`, conservative GC, list/str/map indexing

## What's not supported

- Multiple inheritance, MRO
- Decorators (`@property`, `@staticmethod`, `@classmethod`, …)
- Generators, `yield`, `async` / `await`
- Exceptions (`try` / `except` / `raise` / `finally`)
- Dynamic typing — every name needs an annotation at first binding
- `*args`, `**kwargs`, default arguments
- Lambdas, nested / closure functions
- Comprehensions (list / dict / set / generator)
- Metaclasses, `__new__`, `__slots__`, descriptors
- Sets, tuples, bytes, bytearray
- Context managers (`with`)
- `eval`, `exec`, `getattr` / `setattr` / `hasattr`
- Monkey-patching — classes are closed after definition
- Most of the standard library

## Repository layout

```
cmd/spython   CLI entry point (build / run)
lexer         Tokenizer
parser        AST + parser
loader        Module loading and dependency resolution
types         Type checker and type environment
codegen       LLVM IR emitter (raw text, no bindings)
runtime       runtime.c — single C file linked into every binary
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
