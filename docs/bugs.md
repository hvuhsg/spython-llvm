# Bugs encountered while writing the benchmark suite

Written 2026-04-27 against the spython binary built from `main` at commit
`0d55590`. Each entry has a minimal repro, observed behavior, expected
behavior, and a guess at the underlying cause based on the symptoms.

These are independent of the benchmark PRs themselves; the benchmarks
work around them. They're listed in roughly the order they were hit.

---

## 1. Reading a global `int`/`float` from inside a function emits malformed IR

### Repro

```python
MAX_ITER: int = 1000

def f() -> int:
    i: int = 0
    while i < MAX_ITER:
        i = i + 1
    return i

print(f())
```

### Observed

Codegen emits:

```
%t375 = icmp slt i64 %t374, @spy_main_MAX_ITER
```

clang then rejects with `error: global variable reference must have
pointer type`. Same shape with `float` globals:

```
store double @spy_main_SOLAR_MASS, double* %t749
```

### Expected

The global symbol should be loaded into an SSA value before being
compared/stored, e.g.:

```
%t373 = load i64, i64* @spy_main_MAX_ITER
%t375 = icmp slt i64 %t374, %t373
```

### Likely cause

The codegen path for a `Name` reference inside an expression assumes
the symbol resolves to an SSA register. For module-scope `int`/`float`
globals it returns the global's address as if it were a value.
Probably missing a `load` emission in the `int`/`float` global branch
of name resolution.

### Workaround used in benchmarks

`benchmarks/nbody.spy` and `benchmarks/mandelbrot.spy` inline the
constants as literals at every use site inside functions. Module-scope
reads of the same globals work, so it's specifically the in-function
read path.

---

## 2. `fannkuch(n=9)` and above segfault in the compiled binary

### Repro

`benchmarks/fannkuch.spy` with `print(fannkuch(9))` (or `10`). Same
file with `fannkuch(8)` runs to completion in milliseconds.

### Observed

The compiled binary exits with signal 11 (139). CPython runs the
identical source to completion and prints `30` for `n=9`, `38` for
`n=10`. Symptom appears with both the per-call list copy and a
preallocated scratch buffer, so it's not allocation-pressure-related.

### Suspected

Either:
- a list-bounds-check failure that triggers an undefined branch instead
  of an exception, or
- a bdwgc collection running during a moment where a register holds a
  derived pointer (interior pointer) into a list that gets relocated.

I didn't dig in enough to localize. The trip point is sharp: `n=8` is
fine, `n=9` always fails. n=8 enumerates 8! = 40k permutations; n=9 is
362k. So either a counter overflow somewhere or a heap state crossing a
threshold.

### Workaround

`benchmarks/fannkuch.spy` runs `fannkuch(8)` 20 times and sums.

---

## 3. Inline type annotation on `self.field` is a parse error

### Repro

```python
class Tree:
    def __init__(self):
        self.depth: int = 0
```

### Observed

```
parser: 3:19: unexpected token: : (":")
```

### Expected

PEP 526-style annotated attribute assignment is standard Python 3 syntax
and would help spython infer field types unambiguously rather than
relying on RHS inference.

### Workaround

Drop the annotation: `self.depth = 0`. spython infers the field type
from the RHS literal/expression.

---

## 4. Implicit line continuation across parentheses doesn't parse

### Repro

```python
b: Body = Body(
    1.0,
    2.0,
    3.0,
)
```

### Observed

```
parser: 1:22: unexpected token: NEWLINE ("\n")
```

### Expected

Standard Python lexer treats newlines inside `()`/`[]`/`{}` as ignored
whitespace (PEP 8 calls it implicit line joining). spython treats them
as statement-ending newlines.

### Workaround

Single-line every multi-arg constructor or call. Painful for benchmarks
like `nbody.spy` where the original Benchmarks Game version has each
body's seven arguments on their own line for readability.

---

## 5. `print(float)` truncates to ~6 significant figures

### Repro

```python
print(0.1 + 0.2)
```

### Observed

`0.3` (or `0.30000` depending on path).

### Expected (CPython behavior)

`0.30000000000000004` — repr-style rounding that round-trips to the
same float.

### Impact on benchmarks

Doesn't affect timing, but means spython and CPython output for
floating-point benchmarks visibly differ even when the underlying
computation is bit-identical. The benchmark harness times rather than
diffs, so it's a cosmetic issue for these tests, but it's something
that breaks any "diff against expected" testing for float-heavy
programs.

---

## Summary table

| # | Severity | Where | Workaround |
|---|---|---|---|
| 1 | bug — codegen | int/float global reads inside functions | inline constants |
| 2 | bug — runtime | fannkuch(n≥9) segfaults | use n=8, loop outer |
| 3 | papercut — parser | `self.x: T = …` syntax rejected | drop annotation |
| 4 | papercut — parser | implicit line joining in `()` | single-line calls |
| 5 | papercut — runtime | float repr truncates | n/a |

#1 and #2 are real bugs that would block writing certain programs.
#3, #4, and #5 are papercuts that diverge from CPython but are
workable. None block the benchmark suite — every benchmark currently
ships with a workaround in place.
