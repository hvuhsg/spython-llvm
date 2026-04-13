# spython Compiler Design Spec

## Overview

**spython** is a compiler for a statically-typed subset of Python syntax that compiles to LLVM IR and then to native machine code via clang. Written in Go.

The language requires type annotations where the compiler cannot infer types (v1: annotations required everywhere, with the architecture designed to support richer inference later). Memory management uses the Boehm conservative garbage collector.

## Language Specification

### Types

| Type | Description | LLVM Representation |
|------|-------------|---------------------|
| `int` | 64-bit signed integer | `i64` |
| `float` | 64-bit double-precision | `double` |
| `bool` | Boolean | `i1` |
| `str` | Heap-allocated, length-prefixed string | `%str*` (pointer to runtime struct) |
| `list[T]` | Generic growable array | `%list*` (pointer to runtime struct) |
| `map[K, V]` | Generic hash map | `%map*` (pointer to runtime struct) |
| `None` | Unit type (void return) | `void` |

### Operators

**Arithmetic:** `+`, `-`, `*`, `/`, `//` (floor div), `%` (modulo), `**` (power)

**Comparison:** `==`, `!=`, `<`, `>`, `<=`, `>=`

**Boolean:** `and`, `or`, `not` (Python-style keywords, short-circuit evaluation)

**Bitwise:** `&` (AND), `|` (OR), `^` (XOR), `~` (NOT), `<<` (left shift), `>>` (right shift)

**Assignment:** `=`, `+=`, `-=`, `*=`, `/=`

### Syntax

```python
# Variable declarations (type annotation required in v1)
x: int = 42
name: str = "hello"
flag: bool = True
pi: float = 3.14

# If/elif/else
if x > 10:
    print(x)
elif x > 5:
    print(0)
else:
    print(-1)

# While loops
while x > 0:
    x = x - 1

# For loops — range-based
for i in range(10):
    print(i)

# For loops — over collections
for item in my_list:
    print(item)

# Functions (param and return type annotations required)
def add(a: int, b: int) -> int:
    return a + b

# Lists
nums: list[int] = [1, 2, 3]
nums.append(4)
first: int = nums[0]

# Maps
ages: map[str, int] = {"alice": 30, "bob": 25}
ages["carol"] = 28
age: int = ages["alice"]

# Strings
greeting: str = "hello " + "world"
length: int = len(greeting)
ch: str = greeting[0]
```

### Builtins

- `print(*args)` — print values to stdout (supports int, float, bool, str)
- `len(x)` — length of str, list, or map
- `range(stop)` / `range(start, stop)` / `range(start, stop, step)` — integer range iterator
- `list.append(item)` — append to list
- `str()` / `int()` / `float()` — type conversion functions

### Not Included in v1

- Classes/objects
- Imports/modules
- Closures/lambdas
- Exception handling (try/except)
- Generators/yield
- Decorators
- Multiple return values / tuple unpacking
- Set type
- Slice notation

## Compiler Architecture

```
Source (.spy)
     │
     ▼
┌─────────┐    ┌────────┐    ┌─────────────┐    ┌──────────┐    ┌─────────┐
│  Lexer  │───▶│ Parser │───▶│ Type Checker │───▶│ Codegen  │───▶│  clang  │
│ (tokens)│    │ (AST)  │    │ (typed AST)  │    │ (LLVM IR)│    │ (binary)│
└─────────┘    └────────┘    └─────────────┘    └──────────┘    └─────────┘
```

### Package Structure

```
spython/
├── cmd/spython/           # CLI entry point
│   └── main.go
├── lexer/                 # Tokenization
│   ├── lexer.go
│   ├── lexer_test.go
│   └── token.go           # Token types and Token struct
├── parser/                # AST construction
│   ├── parser.go
│   ├── parser_test.go
│   └── ast.go             # AST node types
├── types/                 # Type system and type checker
│   ├── checker.go         # Type checking pass
│   ├── checker_test.go
│   ├── types.go           # Type representations
│   └── env.go             # Scoped type environment
├── codegen/               # LLVM IR generation
│   ├── codegen.go
│   ├── codegen_test.go
│   ├── builtins.go        # print, len, range implementations
│   └── runtime.go         # Runtime type layout definitions
├── errors/                # Shared error types
│   └── errors.go
├── runtime/               # C runtime library
│   ├── runtime.c          # GC init, string/list/map implementations
│   └── runtime.h
├── testdata/              # Integration test files
│   ├── arithmetic.spy
│   ├── arithmetic.expected
│   └── ...
├── go.mod
└── go.sum
```

### Phase Details

#### 1. Lexer (`lexer/`)

Converts source text into a stream of tokens. Indentation-sensitive: maintains an indentation stack and emits synthetic `INDENT` and `DEDENT` tokens when indentation changes, so the parser can be indentation-unaware.

**Token types include:** identifiers, keywords (`if`, `elif`, `else`, `while`, `for`, `in`, `def`, `return`, `and`, `or`, `not`, `True`, `False`, `None`, `range`), literals (int, float, string), operators, delimiters (`:`, `,`, `(`, `)`, `[`, `]`, `{`, `}`, `->`), `INDENT`, `DEDENT`, `NEWLINE`, `EOF`.

**Key behavior:**
- Tracks line and column for every token (for error messages)
- Handles string escapes (`\n`, `\t`, `\\`, `\"`)
- Skips comments (`#` to end of line)
- Emits `NEWLINE` tokens at line boundaries (ignoring blank lines and lines that are only comments)

#### 2. Parser (`parser/`)

Recursive descent parser that consumes tokens and produces an AST. Uses precedence climbing for expressions.

**AST node types:**
- **Statements:** `AssignStmt`, `AugAssignStmt`, `IfStmt`, `WhileStmt`, `ForStmt`, `FuncDef`, `ReturnStmt`, `ExprStmt`
- **Expressions:** `BinaryExpr`, `UnaryExpr`, `CallExpr`, `IndexExpr`, `AttrExpr` (for method calls like `list.append`), `IdentExpr`, `IntLit`, `FloatLit`, `StrLit`, `BoolLit`, `NoneLit`, `ListLit`, `MapLit`
- **Types:** `TypeAnnotation` (with name and optional generic params)

Each node carries a `Pos` (file, line, col) for error reporting. Expression nodes have a `ResolvedType` field that starts nil and is filled in by the type checker.

#### 3. Type Checker (`types/`)

Walks the AST and resolves all types. Annotates each expression node with its concrete type.

**Type representations:**
- `IntType`, `FloatType`, `BoolType`, `StrType`, `NoneType` — primitive types
- `ListType{Elem Type}` — parameterized list
- `MapType{Key Type, Value Type}` — parameterized map
- `FuncType{Params []Type, Return Type}` — function signature

**Scoped environment (`env.go`):**
- Maintains a stack of scopes (global → function → block)
- Each scope maps variable names to their types
- Lookup walks up the scope chain

**Checking rules (v1):**
- All variable declarations must have type annotations
- All function parameters and return types must be annotated
- Binary operators checked for compatible operand types
- Function calls checked against declared signatures
- Index expressions checked (list index must be int, map key must match key type)
- Assignment RHS must match declared variable type
- `for x in range(...)` — loop variable is `int`
- `for x in list_expr` — loop variable type is the list's element type

**Design for extensibility:** The type checker calls a `resolveType(expr)` method that can be extended with inference rules. For v1 it simply reads annotations. Later, this can be enhanced to infer from literals, return expressions, etc.

#### 4. Code Generator (`codegen/`)

Walks the typed AST and emits LLVM IR as text to a `strings.Builder`.

**Strategy:**
- Each spython function becomes an LLVM function
- Top-level statements go into a `@main` function
- Local variables use `alloca` (stack allocation) + `load`/`store`
- Heap types (str, list, map) are pointers to runtime structs, allocated via runtime calls
- Boolean short-circuit: `and`/`or` use conditional branching, not bitwise ops
- For loops over `range()` compile to a counted loop with an `i64` counter
- For loops over lists compile to index-based iteration with bounds checking

**LLVM IR patterns:**
- String literals: global constants with `@.str.N = private unnamed_addr constant`
- Function calls: `call` instruction with appropriate types
- Comparisons: `icmp`/`fcmp` instructions
- Arithmetic: `add`/`sub`/`mul`/`sdiv`/`srem` for int, `fadd`/`fsub`/`fmul`/`fdiv`/`frem` for float. `**` (power) uses `llvm.powi` intrinsic for int, `llvm.pow` for float.
- Control flow: `br` (conditional and unconditional), labels for if/elif/else/while/for

#### 5. C Runtime (`runtime/`)

A small C library compiled and linked into every spython binary. Provides:

**GC:**
- `spy_init()` — initialize Boehm GC (called at start of main)
- All allocations use `GC_malloc` instead of `malloc`

**Strings (`spy_str_*`):**
- Struct: `{i64 len, i8* data}`
- `spy_str_new(data, len)` — create string
- `spy_str_concat(a, b)` — concatenate
- `spy_str_eq(a, b)` — equality comparison
- `spy_str_index(s, i)` — character at index (returns single-char string)
- `spy_str_len(s)` — length
- `spy_str_print(s)` — print to stdout

**Lists (`spy_list_*`):**
- Struct: `{i64 len, i64 cap, i8* data, i64 elem_size}`
- `spy_list_new(elem_size)` — create empty list
- `spy_list_append(list, elem_ptr)` — append element
- `spy_list_get(list, index)` — get element pointer
- `spy_list_set(list, index, elem_ptr)` — set element
- `spy_list_len(list)` — length

**Maps (`spy_map_*`):**
- Simple hash map implementation (open addressing or chaining)
- `spy_map_new(key_size, val_size, hash_fn, eq_fn)` — create empty map
- `spy_map_set(map, key_ptr, val_ptr)` — insert/update
- `spy_map_get(map, key_ptr)` — lookup (returns pointer or null)
- `spy_map_contains(map, key_ptr)` — check existence
- `spy_map_len(map)` — size

**Print:**
- `spy_print_int(x)`, `spy_print_float(x)`, `spy_print_bool(x)`, `spy_print_str(s)`, `spy_print_newline()`

#### 6. CLI (`cmd/spython/`)

```
spython build <file.spy>          # Compile to binary (output: same name without .spy)
spython build -o <output> <file>  # Compile with custom output name
spython run <file.spy>            # Compile to temp binary, run it, delete binary
```

**Build pipeline:**
1. Read source file
2. Lex → Parse → Type Check → Generate LLVM IR
3. Write IR to temp `.ll` file
4. Invoke `clang -O2 <file.ll> runtime/runtime.c -lgc -o <output>` (links Boehm GC)
5. Clean up temp file

## Error Handling

### Error Type

```go
type CompileError struct {
    File    string
    Line    int
    Col     int
    Phase   string  // "lexer", "parser", "type", "codegen"
    Message string
}
```

### Error Display

```
foo.spy:3:12: type error: cannot add str and int
    x = name + 42
               ^^
```

Errors include the source line and a caret/underline pointing to the relevant span. The compiler collects multiple errors per phase before stopping (up to a configurable limit, default 10).

## Testing Strategy

### Unit Tests
- **Lexer:** Input string → expected token sequence (table-driven)
- **Parser:** Token sequence → expected AST structure (table-driven)
- **Type checker:** AST → expected types on expressions, or expected errors
- **Codegen:** Typed AST → expected LLVM IR fragments

### Integration Tests
- `.spy` source files in `testdata/` with corresponding `.expected` files containing expected stdout
- Test runner compiles each, executes the binary, compares stdout
- Error test files (`.spy` that should fail) with `.error` files containing expected error messages

### CI
- `go test ./...` runs all unit tests
- Integration test script compiles and runs all testdata files

## Verification

To verify the compiler works end-to-end:
1. `go build ./cmd/spython` — compiler builds
2. `go test ./...` — all unit tests pass
3. Create a test file exercising all features and run `spython run test.spy`
4. Verify output matches expectations
5. `spython build test.spy` produces a standalone binary that runs correctly
