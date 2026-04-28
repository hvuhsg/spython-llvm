# CPython stdlib parity in spython

An honest accounting of how spython's stdlib relates to CPython's. Names and
purposes match; **signatures generally do not**. This document captures the
gaps so that users (and we) don't mistake "module exists" for "drop-in
replacement."

## TL;DR

- **4 of 18** shipped modules match CPython's public API: `keyword`,
  `errno` (Darwin-pinned values), `stat`, `colorsys`. `fnmatch` remains the
  one outstanding 1:1 candidate; it needs string slicing or a wider extern
  surface to land. `itertools` ships as a generator-based subset
  (callables-arg variants intentionally omitted). `re` shipped this
  revision as a POSIX-ERE-backed subset — name- and shape-compatible with
  CPython's `re` for the common cases, but the recognised regex syntax is
  POSIX ERE, not Python re (no `\d`/`\w`/`\s`, no `(?:...)` /
  `(?=...)`, no named groups).
- Several previously-partial modules grew significantly: `math` now ships
  35 functions and `inf`/`nan` constants (including varargs `gcd`/`lcm`/
  `hypot`); `hashlib` adds `sha224`/`sha384`/`sha512`; `time` adds the four
  `*_ns` clock readers and `perf_counter`/`process_time`; `os` ships the
  POSIX path / open-flag / access constants plus `setenv`/`unsetenv`/
  `umask`/`access`/`cpu_count`; `sys.argv` and `sys.platform` are now
  attributes (matching CPython) and `byteorder`/`maxsize`/`version`/
  `executable` joined them; `ospath.join` accepts `*paths`.
- spython now supports `*args`, `**kwargs`, keyword-only parameters (after
  `*args`), keyword arguments at call sites, `*expr` / `**expr` unpacking
  at call sites, **and default argument values** (positional or keyword-
  only; defaults are inlined at each call site that omits the slot).
  Variadic params are still uniformly typed (`*xs: int`), so
  heterogeneous-tuple APIs like `struct.pack(fmt, *vals)` remain blocked.
- Generators (`yield`, `yield from`, the `Iterator[T]` return type, the
  `next()` builtin, and `StopIteration`) are now supported. Generator
  expressions and `gen.send` / `gen.throw` / `gen.close` are still out of
  scope.
- Everything else is blocked by one or more of: no first-class callables
  / closures, no `with` / context managers, no decorators, no
  metaclasses, no runtime type introspection, static typing.

---

## Already shipped — gaps vs. CPython

| Shipped | What we expose | What CPython exposes that we miss |
|---|---|---|
| `math` | 35 funcs + 5 constants (`pi`, `e`, `tau`, `inf`, `nan`). New since prior revision: `sinh` / `cosh` / `tanh` / `asinh` / `acosh` / `atanh`, `expm1`, `log1p`, `copysign`, `ldexp`, `trunc`, `degrees`, `radians`, `erf`, `erfc`, `gamma`, `lgamma`, `factorial`, `isqrt`, `isfinite`, `isinf`, `isnan`, `gcd(*ints)`, `lcm(*ints)`, `hypot(*coords)`. | `comb`, `perm`, `frexp` / `modf` (return tuples — implementable now), `dist`, `isclose`, `prod`, `nextafter`, `ulp`. With defaults landed, `isclose(a, b, *, rel_tol=1e-9, abs_tol=0.0)` and `prod(it, *, start=1)` are now reachable in shape — pure additive work. |
| `random` | `seed`, `random`, `randint`, `uniform`, `getrandbits` | `randrange(start, stop=None, step=1)`, `choice(seq)`, `choices(pop, weights=None, *, cum_weights, k=1)`, `sample(pop, k, *, counts)`, `shuffle(x)`, `gauss`, `normalvariate`, `triangular(low=0, high=1, mode=None)`, `beta` / `expo` / `gamma` / `lognorm` / `pareto` / `weibull` / `vonmises`-`variate`, `getstate` / `setstate`, the `Random` class. ~5 of ~25 functions; defaults landed, so most of the missing module-level signatures are now reachable in shape. The `Random` class still requires class state. |
| `time` | `time`, `time_ns`, `monotonic`, `monotonic_ns`, `perf_counter`, `perf_counter_ns`, `process_time`, `process_time_ns`, `sleep` | `localtime([t])` / `gmtime([t])` / `mktime` (struct_time namedtuple), `strftime(fmt[, t])` / `strptime(s[, fmt])` (format strings), `asctime`, `ctime`, `clock_gettime`. Defaults are no longer a blocker; format strings + the `struct_time` namedtuple still block real parity for the calendar-related calls. |
| `io` | `open()` returning a `File` class with `read` / `read_n` / `readline` / `write` / `close` | `BytesIO`, `StringIO`, `TextIOWrapper`, `BufferedReader` / `Writer`, the iterator protocol (`for line in f`), `with f:`, `open(..., encoding=, errors=, newline=, closefd=, opener=)`, `seek(offset, whence=0)` default. Even `open` doesn't match — CPython's signature is 8 kwargs. |
| `os` | `getenv`, `getcwd`, `getpid`, `chdir`, `mkdir`, `remove`, `rmdir`, `rename`, `listdir`, `_stat_mode`, `setenv`, `unsetenv`, `umask`, `cpu_count`, `access`, plus the `sep` / `linesep` / `pathsep` / `name` / `curdir` / `pardir` / `extsep` / `devnull` / `F_OK` / `R_OK` / `W_OK` / `X_OK` / `O_RDONLY` / `O_WRONLY` / `O_RDWR` / `O_CREAT` / `O_EXCL` / `O_TRUNC` / `O_APPEND` / `O_NONBLOCK` constants | `environ` dict, `getenv(key, default=None)` (defaults landed — adding the second arg is now reachable), `putenv`, `makedirs(name, mode=0o777, exist_ok=False)` (also reachable now), `stat()` returning `os.stat_result` (10-field namedtuple), `lstat` / `fstat`, `walk()` (generator), `scandir()` (generator → `DirEntry`), `pipe` / `dup` / `dup2` / `fork` / `exec*` / `kill` / `waitpid`, `getuid` / `setuid` family, `urandom`, `popen`, `system`, `path` as a submodule (we expose it as a separate `ospath` module). |
| `os.path` | `join(path, *paths)`, `split`, `basename`, `dirname`, `splitext`, `exists`, `isfile`, `isdir` | `abspath`, `commonpath` / `commonprefix`, `expanduser`, `expandvars`, `getatime` / `getctime` / `getmtime` / `getsize`, `isabs`, `islink`, `ismount`, `normcase`, `normpath`, `realpath`, `relpath(p, start=os.curdir)` (defaults landed — reachable now), `samefile`, `splitdrive`. Plus we named it `ospath` because there's no dotted-package support — so `import os.path` doesn't even resolve. |
| `sys` | `argv` (attribute, mutable list), `platform`, `byteorder`, `maxsize`, `version`, `executable`, `exit`, `stdout_write`, `stderr_write` | `version_info` (named tuple), `path` (import-search list), `modules` dict, `stdin` / `stdout` / `stderr` as file-like objects, `int_info` / `float_info` / `hash_info`, `getrecursionlimit` / `setrecursionlimit`, `excepthook`, `_getframe`, `gettrace` / `settrace`. `version` is a spython-style string, not a CPython release string. |
| `hashlib` | `md5(data) → hex`, `sha1(data) → hex`, `sha224(data) → hex`, `sha256(data) → hex`, `sha384(data) → hex`, `sha512(data) → hex` | `sha3_*` / `blake2b` / `blake2s` / `shake_*`, `new(name[, data])` factory, `algorithms_available` / `guaranteed`, `pbkdf2_hmac(name, pw, salt, iters, dklen=None)`, `scrypt`. Plus the entire **hash object** API: `h = sha256(); h.update(b); h.digest(); h.hexdigest(); h.copy(); h.digest_size; h.block_size; h.name`. We return a hex string instead of a hash object — a fundamental API break, not a gap. |
| `binascii` | `hexlify`, `unhexlify`, `crc32` | `a2b_uu` / `b2a_uu`, `a2b_base64(*, strict_mode=False)` / `b2a_base64(*, newline=True)`, `a2b_qp` / `b2a_qp`, `crc_hqx`, `hexlify(data[, sep[, bytes_per_sep]])` (defaults landed — reachable now), `b2a_hex` / `a2b_hex` aliases, `Error` / `Incomplete` exceptions. Also: ours takes `str`, CPython takes `bytes`. |
| `base64` | `b64encode(bytes) → str`, `b64decode(str) → bytes` | `altchars` parameter on both, `validate=False` (now reachable), `standard_b64encode/decode`, `urlsafe_b64encode/decode`, `b32encode` / `b32decode(s, casefold=False, map01=None)`, `b32hexencode`, `b16encode` / `b16decode(s, casefold=False)`, `a85encode(b, *, foldspaces, wrapcol, pad, adobe)`, `a85decode`, `b85encode` / `b85decode`, `encode(input, output)` (file-like), `encodebytes` / `decodebytes`. Defaults are no longer the blocker; the `bytes`-vs-`str` API divergence and file-like inputs still are. Also: invalid input silently returns empty bytes instead of raising `binascii.Error`. |
| `struct` | `pack_<type>_<endian>(ba, v)` / `unpack_<type>_<endian>(b, off)` | The **entire** CPython API: `pack(fmt, *values)`, `unpack(fmt, buf)`, `pack_into`, `unpack_from`, `iter_unpack`, `calcsize`, `Struct` class. `*values` is varargs (now syntactically supported) but spython's varargs are uniformly typed — `pack` needs a heterogeneous tuple of ints / floats / bytes determined by `fmt`, which we still can't express. Already documented as a deliberate departure. |
| `socket` | `Socket` class: `__init__(family, type)`, `connect`, `bind`, `listen`, `accept`, `shutdown`, `send`, `sendall`, `recv`, `sendto`, `recvfrom`, `getsockname`, `getpeername`, `setsockopt(level, name, int)` / `getsockopt(level, name) -> int`, `setblocking`, `settimeout`, `fileno`, `close`. Module: `socket`, `gethostname`, `gethostbyname`, `gaierror`. Constants: `AF_INET`, `SOCK_STREAM` / `SOCK_DGRAM`, `SOL_SOCKET`, `SO_REUSEADDR` / `SO_KEEPALIVE` / `SO_BROADCAST` / `SO_RCVTIMEO` / `SO_SNDTIMEO` / `SO_RCVBUF` / `SO_SNDBUF` / `SO_ERROR` / `SO_TYPE` / `SO_REUSEPORT`, `IPPROTO_TCP` / `IPPROTO_UDP`, `SHUT_RD` / `SHUT_WR` / `SHUT_RDWR`. Errors: `OSError`, `ConnectionRefusedError`, `ConnectionResetError`, `ConnectionAbortedError`, `BrokenPipeError`, `BlockingIOError`, `TimeoutError`, `gaierror`. | `getaddrinfo(host, port, family=0, type=0, proto=0, flags=0)` — defaults landed, but the heterogeneous 5-tuple sockaddr in the return is still blocked; `getsockopt(name, buflen)` for byte-buffer options, `makefile` / `fromfd` / `socketpair`, `recv_into` / `recvfrom_into`, IPv6 (`AF_INET6`), Unix (`AF_UNIX`), the dozens of remaining `SO_*` / `IPPROTO_*` / `IP_*` / `TCP_*` constants, `socket.error` (alias of OSError), `herror`, `socket.timeout` (we raise builtin `TimeoutError`). `setsockopt`/`getsockopt` are int-valued only — byte-buffer options aren't expressible without `bytes`-typed extern args. `gethostbyname` only returns the first IPv4 (no `gethostbyname_ex` triple). |
| `keyword` | `kwlist`, `softkwlist`, `iskeyword(s)` | None — full parity with CPython 3.13. |
| `errno` | All 106 `errno.h` integer constants on Darwin/BSD, `EWOULDBLOCK` alias, `errorcode` reverse-lookup map | Values are pinned to the Darwin/macOS build host; on Linux the integers differ (notably `EAGAIN=11`, `EINPROGRESS=115`). Names and `errorcode` semantics are identical. CPython annotates `errorcode` as `dict[int, str]`; spython uses `map[int, str]` for the same type. |
| `stat` | `S_IFMT` / `S_IMODE` functions, file-type constants, file-type predicates (`S_ISDIR`, `S_ISREG`, …), permission-bit constants, `filemode(mode)` | None — output matches CPython byte-for-byte across all common modes. |
| `colorsys` | `rgb_to_yiq` / `yiq_to_rgb` / `rgb_to_hls` / `hls_to_rgb` / `rgb_to_hsv` / `hsv_to_rgb` | None — coefficients match CPython exactly; hue normalization works around spython's C-style float `%`. |
| `itertools` | `count(start, step)`, `repeat(value, times)` / `repeat_forever(value)`, `cycle(xs)`, `chain(its)` / `chain_lists(xss)`, `islice(it, stop)` / `islice_range(it, start, stop, step)`, `accumulate(xs)`, `pairwise(xs)`, `compress(data, selectors)`, `batched(xs, n)`, `tee(it)`, `combinations(xs, r)`, `permutations(xs, r)` | All callable-arg variants (`filterfalse`, `dropwhile`, `takewhile`, `starmap`, `groupby`, `accumulate(func=...)`) — closures still missing. `chain(*iterables)` becomes `chain(its)` (varargs of generator params not yet allowed). Polymorphic iterators are int-specialized. `tee` returns two materialized lists rather than two lazy views. `product`, `zip_longest` need heterogeneous tuple typing. |
| `re` | `search` / `match` / `fullmatch` returning a `Match`, `findall`, `finditer`, `sub` / `subn`, `split`, `escape`, `IGNORECASE` / `MULTILINE` (and aliases `I` / `M`); `error` exception class; `Match.group(i=0)` / `start` / `end` / `span` / `groups()` / `.matched` / `.string`. Backed by libc's POSIX ERE engine (regcomp/regexec) — no extra link flags. Single-slot compile cache amortises repeated `_exec` calls inside findall/finditer/sub. | Engine is POSIX ERE, so the recognised pattern syntax is **not** Python re: no `\d` / `\w` / `\s` / `\b`, no `(?:...)` / `(?=...)` / `(?!...)` / `(?<...)`, no `(?P<name>...)` named groups, no inline `(?i)` flags, no in-pattern backreferences (POSIX BRE has them, ERE doesn't). `re.DOTALL` is accepted but is a noop — POSIX `.` matches `\n` by default and `MULTILINE` (= `REG_NEWLINE`) flips both `^/$` *and* dot-vs-newline in lockstep. `re.compile` / `re.Pattern` are not shipped (a single-slot cache covers the common loop case). `re.search` returns a Match with `.matched=False` instead of `None` (no Optional sugar yet). `findall` always returns the full-match text; CPython returns capture-group tuples when groups exist, but `list[str]` is homogeneous. Callable `repl` for `sub` is blocked on closures. Embedded NUL bytes truncate the search (POSIX regexec is null-terminated). |

---

## Initially-promising modules that still turn out partial

With defaults landed, several of these became newly reachable in shape —
the table below splits "newly reachable (additive work only)" from
"still blocked on a deeper feature."

### Newly reachable (defaults landed — pure additive port from CPython)

| Module | Notes |
|---|---|
| `textwrap` | Every function takes ≥6 keyword args (`width=70, initial_indent='', …`). Defaults were the only blocker. |
| `uuid` | `UUID(hex=None, bytes=None, bytes_le=None, fields=None, int=None, version=None, *, is_safe=...)` is now syntactically expressible end-to-end. Class state + the `bytes` type still need work, but the signature is reachable. |
| `secrets` | Every function had a default arg (`token_bytes(nbytes=None)`); now reachable. |
| `calendar` | `Calendar(firstweekday=0)`, `month(theyear, themonth, w=0, l=0)`, `prmonth(theyear, …)` — defaults were the blocker. |
| `getopt` | `getopt(args, shortopts, longopts=[])` — default was the blocker. |

### Still blocked on a deeper feature

| Module | Actual blocker |
|---|---|
| `cmath` | Returns native `complex` — we have no complex type. (`isclose` / `log(z[, base])` defaults are no longer a blocker.) |
| `string` | `Template.substitute(**kwargs)` values must all share one type — CPython accepts arbitrary objects. `Formatter` would still need first-class callables for custom converters. Constants + `capwords` are fine. |
| `heapq` | `merge(*iterables, key=None, reverse=False)` — defaults landed, but `key=` is a **callable kwarg** and `merge` is a **generator**; `nlargest` / `nsmallest(n, it, key=None)` still need a callable. |
| `bisect` | `bisect_left(a, x, lo=0, hi=None, *, key=None)` — defaults landed; `key=` is a **callable kwarg**. |
| `glob` | `glob(pat, *, recursive=False, root_dir=None, dir_fd=None, include_hidden=False)` defaults + the `iglob` generator are no longer blockers, but the implementation still needs `fnmatch`-style pattern matching (which itself isn't shipped — see below). |
| `mimetypes` | Module-level functions are fine; `MimeTypes` class needs `read` / `readfp` taking file-like objects. |
| `ipaddress` | `ip_address(addr)` accepts `int \| str \| bytes` (union types); constructors take many kwargs. |
| `hmac` | `new(key, msg=None, digestmod='')` — `digestmod` accepts a string OR a callable OR a module (union + first-class callables). |
| `mmap`, `select`, `signal`, `subprocess`, `shutil`, `sqlite3`, `ssl`, `zlib` / `gzip`, `datetime`, `pathlib`, `argparse`, `csv` | Generator returns, callable kwargs, context manager use, or class-state-heavy APIs — all still pervasive even after defaults. |

---

## Modules that genuinely can match CPython 1:1 today

Four of the original five 1:1 candidates now ship (see "Already shipped"
above): `keyword`, `errno`, `stat`, `colorsys`. The remaining one is:

| Module | Public surface | Notes |
|---|---|---|
| `fnmatch` | `fnmatch(name, pat)`, `fnmatchcase(name, pat)`, `filter(names, pat)`, `translate(pat)` | Signatures are fixed-positional, but the implementation needs string slicing (or a substantial extern surface) to walk the pattern, and `translate` produces a regex that the runtime would need a matcher for. Reachable, just not landed. |

Landing the four already done required relaxing the loader's
`isConstExpr` check to admit list/tuple/map literals at module top level,
plus a `_pymod` helper in `colorsys` to bridge spython's C-style float
modulo with CPython's `(0, m]` semantics. No deeper compiler work was
needed.

---

## Not supportable at all without compiler changes

Each is blocked by a specific feature spython intentionally omits.

| Module | Blocker |
|---|---|
| `asyncio`, `contextvars` | `async` / `await`, context managers. |
| `enum` | Metaclass-driven class creation; `Enum` members are class-attribute introspection magic. |
| `dataclasses` | Decorators that synthesize `__init__` / `__eq__` at class-definition time. |
| `abc` | Metaclasses (`ABCMeta`) and decorator-marked abstract methods. |
| `typing` (runtime parts: `get_type_hints`, `Protocol` runtime checks, `TypedDict`) | Runtime type introspection; spython has no type objects at runtime. |
| `pickle`, `shelve`, `marshal`, `copyreg` | Pickle is dynamically typed on the wire — must reconstruct *any* class, needs runtime class lookup and dynamic attribute set. |
| `inspect` | Source / frame / object introspection; spython compiles the tree away. |
| `dis` | Bytecode disassembly — there is no bytecode. |
| `ast`, `symtable`, `tokenize`, `parser`, `compile()` | A Python frontend in Python; spython doesn't carry one in the runtime. |
| `importlib`, `pkgutil`, `runpy`, `zipimport` | Dynamic import — modules are statically linked. |
| `pdb`, `bdb` | Debugger frame hooks. |
| `cProfile`, `profile`, `pstats` | Frame instrumentation. |
| `trace` | Line-by-line execution hooks. |
| `ctypes`, `cffi` (stdlib `_ctypes`) | Runtime FFI; spython's FFI is compile-time-resolved (`@extern`). |
| `multiprocessing` | Depends on `pickle` for cross-process object transfer. |
| `concurrent.futures` | Depends on `multiprocessing` for `ProcessPoolExecutor`; `ThreadPoolExecutor` would still need `Future` callbacks (closures) and `with`. |
| `tkinter`, `turtle`, IDLE | Tcl/Tk callbacks need closures. |
| `curses` | `curses.wrapper(fn)` and panel callbacks rely on first-class function values used as closures. |

---

## Headline blockers, ranked by reach

How many modules each missing feature would unlock if added:

1. **Closures / function values as data** — unlocks `key=` everywhere
   (`sorted`, `heapq`, `min` / `max`), `functools.partial`, `defaultdict`,
   `re.sub` callable replacements, threading / server callbacks, signal
   handlers carrying state.
2. **Decorators** — unlocks `dataclasses`, `functools.cache`, `@property`,
   `unittest` skip markers, much of `logging` configuration ergonomics.
3. **`with` / context managers** — ergonomic, not API-blocking; underlying
   classes work, callers just write `try` / `finally`.
4. **Metaclasses / runtime class creation** — unlocks `enum`, `abc`,
   `namedtuple`, ORM-style libs.
5. **Dynamic typing / `Any`** — unlocks `pickle`, generic `copy.deepcopy`,
   schema-free `json`, and the heterogeneous-tuple side of `struct` /
   `string.Template`.

Generators (`yield`, `yield from`, `Iterator[T]`, `next()`,
`StopIteration`) shipped in the prior revision and a generator-based
`itertools` (count / repeat / cycle / chain / islice / accumulate /
pairwise / compress / batched / tee / combinations / permutations)
shipped this revision. **Default arguments shipped this revision as
well** — they are inlined at each call site that omits the slot, so any
signature whose only blocker was defaults is now reachable in shape
(`textwrap`, `secrets`, `uuid`, `calendar`, `getopt`, plus the
default-only signatures inside already-shipped modules like
`os.getenv(k, default)`, `os.path.relpath(p, start=...)`,
`math.isclose`, `math.prod`, `binascii.hexlify(data, sep, …)`, etc.).
Callable-arg variants (`filterfalse`, `dropwhile`, `takewhile`,
`starmap`, `groupby`, `accumulate(func=...)`) are still blocked on
closures; the iterator surface of `csv` / `io` / `os` / `xml` / `email`
/ `re.finditer` is also unblocked. Closures are now the largest
remaining lever.

---

## Honest framing

spython's stdlib is **name-compatible and purpose-compatible** with
CPython, and signature-compatible for an expanding subset. Generators,
varargs / `**kwargs` / keyword-only params, call-site keyword passing
with `*` / `**` unpacking, **and default argument values** all work
now. The remaining gap to full signature parity is mostly first-class
callables (closures), decorators, context managers, metaclasses, and
dynamic typing. Until those land, users still shouldn't expect
arbitrary CPython snippets like `sorted(xs, key=lambda x: x.name)` or
`os.walk(".")` to drop in unchanged.

---

## Priority list (ranked by programmer-user need)

The ordering above is by *implementation lever* — how many modules a
compiler feature unlocks. This section is the inverse: what a working
Python programmer reaches for most often, and where each item stands
today. Use this to decide what to ship next.

### Tier 1 — core daily tools (you cannot write real programs without these)

| Rank | Need | Status | Gating work |
|---|---|---|---|
| 1 | **`re` — regular expressions** | **Shipped this revision** (POSIX-ERE backend; see "Already shipped" above). | Remaining gaps vs. CPython are syntactic (POSIX ERE vs. Python re — no `\d`/`\w`/`\s`, no `(?:...)`, no named groups), plus callable-`repl` for `sub` (closures) and `re.compile` / `Pattern` (cache + class state). |
| 2 | **`json` — JSON encode/decode** | **Not shipped.** | The blocker is **dynamic typing** — CPython returns `dict[str, Any]` from `loads`. A typed-schema variant (`json.loads_into(T, s)`) is reachable today and would cover most real use. The free-form `loads` waits on `Any`. |
| 3 | **`datetime`** | Blocked. | Needs class methods, ample defaults (now landed), and `timedelta` arithmetic via `__add__` / `__sub__` (operator overloads on user classes — check whether shipped). The data shape itself is fine. Should be reachable in stages. |
| 4 | **`pathlib.Path`** | Blocked. | Operator overload (`/`), method chaining, and `__fspath__` protocol. The path operations themselves all exist in `ospath`. Largely a rewrap. |
| 5 | **`argparse`** | Blocked. | Heterogeneous argument values (`type=int`, `action=...`), callable kwargs, and dynamic attribute access on `Namespace`. Needs closures + `Any`. A typed-builder variant could ship sooner. |
| 6 | **`logging`** | Blocked. | Class hierarchy + global config + handler callbacks. Needs closures for handlers and decorators for ergonomic use. A barebones `log.info(s)` / `log.error(s)` module-level shim is reachable today. |
| 7 | **`collections.defaultdict` / `Counter` / `OrderedDict` / `deque`** | Not shipped. | `defaultdict` needs a callable factory (closures). `Counter` and `OrderedDict` are reachable today as plain classes with fixed-type values. `deque` is a straight port. |
| 8 | **`io` — iteration + `with`** | Partially shipped (`open` returning `File`). | `for line in f:` needs the iterator protocol on `File`; `with f:` needs context managers. Both are real ergonomic blockers. |

### Tier 2 — heavy-use convenience (programs are noticeably worse without them)

| Rank | Need | Status | Gating work |
|---|---|---|---|
| 9 | **`functools.partial` / `lru_cache` / `reduce`** | Blocked. | `partial` and `reduce` need first-class callables; `lru_cache` needs decorators. None reachable without closures. |
| 10 | **`itertools` callable variants** (`filterfalse`, `dropwhile`, `takewhile`, `starmap`, `groupby`) | Blocked on closures. | Generator infra is in place; only the callable-arg surface is missing. |
| 11 | **`csv`** | Blocked. | `csv.reader` is a generator over rows (reachable now); `csv.DictReader` needs `Any`-typed values. The reader form is a clean shipping target. |
| 12 | **`subprocess.run` / `Popen`** | Blocked. | Heterogeneous kwargs (`stdin=...`, `capture_output=True`, `text=True`), context manager use, and the `bytes` type. Defaults landed; bytes + class state are the remainder. |
| 13 | **`shutil.copy*` / `rmtree` / `which`** | Not shipped. | Pure additive — the underlying `os` / `ospath` calls all exist. Mostly a thin wrapper module. |
| 14 | **`enum`** | Blocked. | Metaclass-driven. No path without metaclasses. A `const`-style alternative could ship instead. |
| 15 | **`dataclasses`** | Blocked. | Decorator-driven `__init__` synthesis. No path without decorators. |
| 16 | **`typing` (compile-time hints)** | Mostly N/A — spython types are checked statically already. `Optional[T]`, `list[T]`, `dict[K,V]` already work. Runtime introspection (`get_type_hints`, `Protocol` checks) is permanently out of reach without runtime type objects. |

### Tier 3 — specialized but valuable

| Rank | Need | Status | Gating work |
|---|---|---|---|
| 17 | **`urllib.request` / `http.client`** | Not shipped. | Needs HTTP parsing + TLS. Sockets are in place; `ssl` is blocked. Plain HTTP is reachable today. |
| 18 | **`unittest`** | Blocked. | Decorators (`@skip`), introspection (test discovery), and class-method dispatch for `setUp` / `tearDown`. |
| 19 | **`pickle` / `shelve`** | Permanently blocked without `Any`. | Wire format is dynamically typed by definition. |
| 20 | **`asyncio`** | Permanently blocked without `async`/`await` + closures. | Whole-language feature, not a stdlib question. |
| 21 | **`threading.Lock` / `Thread`** | Not shipped. | Threads themselves need a runtime model decision; `Lock` would need a target callable for the thread body (closures). |
| 22 | **`xml.etree`, `email.parser`, `html.parser`** | Not shipped. | Tree types are heterogeneous (`Any`). Iterator surfaces are reachable; trees are not. |

### What to ship next, opinionated

If the goal is **maximum programmer-impact per unit of work**, the order is:

1. **`shutil` thin wrapper** — trivial; high daily value.
2. **`Counter` / `OrderedDict` / `deque` in `collections`** — straight ports.
3. **Iterator protocol on `File`** (`for line in f:`) — small compiler change, large ergonomic win.
4. **Closures** — after the above, this is the single feature that flips the largest remaining set (Tier 2 ranks 9, 10, 11, plus `key=` everywhere, plus callable-`repl` in `re.sub`) from "blocked" to "easy port."
5. **`with` statement desugar** — sugar over `try` / `finally`; modest compiler work, big readability gain.
6. **Decorators** — last of the big four, unlocks `dataclasses` / `lru_cache` / `unittest` markers.

Everything past that (metaclasses, `Any`, `async`) is a much bigger
language commitment and should follow user demand, not the parity
ranking.
