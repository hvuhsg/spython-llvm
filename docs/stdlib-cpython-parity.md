# CPython stdlib parity in spython

An honest accounting of how spython's stdlib relates to CPython's. Names and
purposes match; **signatures generally do not**. This document captures the
gaps so that users (and we) don't mistake "module exists" for "drop-in
replacement."

## TL;DR

- **4 of 17** shipped modules match CPython's public API: `keyword`,
  `errno` (Darwin-pinned values), `stat`, `colorsys`. `fnmatch` remains the
  one outstanding 1:1 candidate; it needs string slicing or a wider extern
  surface to land. `itertools` ships as a generator-based subset
  (callables-arg variants intentionally omitted).
- Several previously-partial modules grew significantly: `math` now ships
  35 functions and `inf`/`nan` constants (including varargs `gcd`/`lcm`/
  `hypot`); `hashlib` adds `sha224`/`sha384`/`sha512`; `time` adds the four
  `*_ns` clock readers and `perf_counter`/`process_time`; `os` ships the
  POSIX path / open-flag / access constants plus `setenv`/`unsetenv`/
  `umask`/`access`/`cpu_count`; `sys.argv` and `sys.platform` are now
  attributes (matching CPython) and `byteorder`/`maxsize`/`version`/
  `executable` joined them; `ospath.join` accepts `*paths`.
- spython now supports `*args`, `**kwargs`, keyword-only parameters (after
  `*args`), keyword arguments at call sites, and `*expr` / `**expr`
  unpacking at call sites. Variadic params are still uniformly typed
  (`*xs: int`), so heterogeneous-tuple APIs like `struct.pack(fmt, *vals)`
  remain blocked.
- Generators (`yield`, `yield from`, the `Iterator[T]` return type, the
  `next()` builtin, and `StopIteration`) are now supported. Generator
  expressions and `gen.send` / `gen.throw` / `gen.close` are still out of
  scope.
- Everything else is blocked by one or more of: no default arguments,
  no first-class callables / closures, no `with` /
  context managers, no decorators, no metaclasses, no runtime type
  introspection, static typing.

---

## Already shipped — gaps vs. CPython

| Shipped | What we expose | What CPython exposes that we miss |
|---|---|---|
| `math` | 35 funcs + 5 constants (`pi`, `e`, `tau`, `inf`, `nan`). New since prior revision: `sinh` / `cosh` / `tanh` / `asinh` / `acosh` / `atanh`, `expm1`, `log1p`, `copysign`, `ldexp`, `trunc`, `degrees`, `radians`, `erf`, `erfc`, `gamma`, `lgamma`, `factorial`, `isqrt`, `isfinite`, `isinf`, `isnan`, `gcd(*ints)`, `lcm(*ints)`, `hypot(*coords)`. | `comb`, `perm`, `frexp` / `modf` (return tuples — implementable now), `dist`, `isclose(a, b, *, rel_tol, abs_tol)` (defaults block), `prod(it, *, start=1)` (default blocks), `nextafter`, `ulp`. Default-arg entries can never match the API exactly. |
| `random` | `seed`, `random`, `randint`, `uniform`, `getrandbits` | `randrange(start, stop=None, step=1)` (defaults), `choice(seq)`, `choices(pop, weights=None, *, cum_weights, k=1)`, `sample(pop, k, *, counts)`, `shuffle(x)`, `gauss`, `normalvariate`, `triangular(low=0, high=1, mode=None)`, `beta` / `expo` / `gamma` / `lognorm` / `pareto` / `weibull` / `vonmises`-`variate`, `getstate` / `setstate`, the `Random` class. ~5 of ~25 functions, none with their CPython default-arg signatures. |
| `time` | `time`, `time_ns`, `monotonic`, `monotonic_ns`, `perf_counter`, `perf_counter_ns`, `process_time`, `process_time_ns`, `sleep` | `localtime([t])` / `gmtime([t])` / `mktime` (struct_time namedtuple), `strftime(fmt[, t])` / `strptime(s[, fmt])` (format strings + defaults), `asctime`, `ctime`, `clock_gettime`. Format strings + namedtuple still block real parity for the calendar-related calls. |
| `io` | `open()` returning a `File` class with `read` / `read_n` / `readline` / `write` / `close` | `BytesIO`, `StringIO`, `TextIOWrapper`, `BufferedReader` / `Writer`, the iterator protocol (`for line in f`), `with f:`, `open(..., encoding=, errors=, newline=, closefd=, opener=)`, `seek(offset, whence=0)` default. Even `open` doesn't match — CPython's signature is 8 kwargs. |
| `os` | `getenv`, `getcwd`, `getpid`, `chdir`, `mkdir`, `remove`, `rmdir`, `rename`, `listdir`, `_stat_mode`, `setenv`, `unsetenv`, `umask`, `cpu_count`, `access`, plus the `sep` / `linesep` / `pathsep` / `name` / `curdir` / `pardir` / `extsep` / `devnull` / `F_OK` / `R_OK` / `W_OK` / `X_OK` / `O_RDONLY` / `O_WRONLY` / `O_RDWR` / `O_CREAT` / `O_EXCL` / `O_TRUNC` / `O_APPEND` / `O_NONBLOCK` constants | `environ` dict, `getenv(key, default=None)` default, `putenv`, `makedirs(name, mode=0o777, exist_ok=False)`, `stat()` returning `os.stat_result` (10-field namedtuple), `lstat` / `fstat`, `walk()` (generator), `scandir()` (generator → `DirEntry`), `pipe` / `dup` / `dup2` / `fork` / `exec*` / `kill` / `waitpid`, `getuid` / `setuid` family, `urandom`, `popen`, `system`, `path` as a submodule (we expose it as a separate `ospath` module). |
| `os.path` | `join(path, *paths)`, `split`, `basename`, `dirname`, `splitext`, `exists`, `isfile`, `isdir` | `abspath`, `commonpath` / `commonprefix`, `expanduser`, `expandvars`, `getatime` / `getctime` / `getmtime` / `getsize`, `isabs`, `islink`, `ismount`, `normcase`, `normpath`, `realpath`, `relpath(p, start=os.curdir)` (default blocks), `samefile`, `splitdrive`. Plus we named it `ospath` because there's no dotted-package support — so `import os.path` doesn't even resolve. |
| `sys` | `argv` (attribute, mutable list), `platform`, `byteorder`, `maxsize`, `version`, `executable`, `exit`, `stdout_write`, `stderr_write` | `version_info` (named tuple), `path` (import-search list), `modules` dict, `stdin` / `stdout` / `stderr` as file-like objects, `int_info` / `float_info` / `hash_info`, `getrecursionlimit` / `setrecursionlimit`, `excepthook`, `_getframe`, `gettrace` / `settrace`. `version` is a spython-style string, not a CPython release string. |
| `hashlib` | `md5(data) → hex`, `sha1(data) → hex`, `sha224(data) → hex`, `sha256(data) → hex`, `sha384(data) → hex`, `sha512(data) → hex` | `sha3_*` / `blake2b` / `blake2s` / `shake_*`, `new(name[, data])` factory, `algorithms_available` / `guaranteed`, `pbkdf2_hmac(name, pw, salt, iters, dklen=None)`, `scrypt`. Plus the entire **hash object** API: `h = sha256(); h.update(b); h.digest(); h.hexdigest(); h.copy(); h.digest_size; h.block_size; h.name`. We return a hex string instead of a hash object — a fundamental API break, not a gap. |
| `binascii` | `hexlify`, `unhexlify`, `crc32` | `a2b_uu` / `b2a_uu`, `a2b_base64(*, strict_mode=False)` / `b2a_base64(*, newline=True)`, `a2b_qp` / `b2a_qp`, `crc_hqx`, `hexlify(data[, sep[, bytes_per_sep]])` (defaults), `b2a_hex` / `a2b_hex` aliases, `Error` / `Incomplete` exceptions. Also: ours takes `str`, CPython takes `bytes`. |
| `base64` | `b64encode(bytes) → str`, `b64decode(str) → bytes` | `altchars` parameter on both, `validate=False`, `standard_b64encode/decode`, `urlsafe_b64encode/decode`, `b32encode` / `b32decode(s, casefold=False, map01=None)`, `b32hexencode`, `b16encode` / `b16decode(s, casefold=False)`, `a85encode(b, *, foldspaces, wrapcol, pad, adobe)`, `a85decode`, `b85encode` / `b85decode`, `encode(input, output)` (file-like), `encodebytes` / `decodebytes`. Also: invalid input silently returns empty bytes instead of raising `binascii.Error`. |
| `struct` | `pack_<type>_<endian>(ba, v)` / `unpack_<type>_<endian>(b, off)` | The **entire** CPython API: `pack(fmt, *values)`, `unpack(fmt, buf)`, `pack_into`, `unpack_from`, `iter_unpack`, `calcsize`, `Struct` class. `*values` is varargs (now syntactically supported) but spython's varargs are uniformly typed — `pack` needs a heterogeneous tuple of ints / floats / bytes determined by `fmt`, which we still can't express. Already documented as a deliberate departure. |
| `socket` | `Socket` class: `__init__(family, type)`, `connect`, `bind`, `listen`, `accept`, `shutdown`, `send`, `sendall`, `recv`, `sendto`, `recvfrom`, `getsockname`, `getpeername`, `setsockopt(level, name, int)` / `getsockopt(level, name) -> int`, `setblocking`, `settimeout`, `fileno`, `close`. Module: `socket`, `gethostname`, `gethostbyname`, `gaierror`. Constants: `AF_INET`, `SOCK_STREAM` / `SOCK_DGRAM`, `SOL_SOCKET`, `SO_REUSEADDR` / `SO_KEEPALIVE` / `SO_BROADCAST` / `SO_RCVTIMEO` / `SO_SNDTIMEO` / `SO_RCVBUF` / `SO_SNDBUF` / `SO_ERROR` / `SO_TYPE` / `SO_REUSEPORT`, `IPPROTO_TCP` / `IPPROTO_UDP`, `SHUT_RD` / `SHUT_WR` / `SHUT_RDWR`. Errors: `OSError`, `ConnectionRefusedError`, `ConnectionResetError`, `ConnectionAbortedError`, `BrokenPipeError`, `BlockingIOError`, `TimeoutError`, `gaierror`. | `getaddrinfo(host, port, family=0, type=0, proto=0, flags=0)` (defaults; returns list of 5-tuples with heterogeneous sockaddr), `getsockopt(name, buflen)` for byte-buffer options, `makefile` / `fromfd` / `socketpair`, `recv_into` / `recvfrom_into`, IPv6 (`AF_INET6`), Unix (`AF_UNIX`), the dozens of remaining `SO_*` / `IPPROTO_*` / `IP_*` / `TCP_*` constants, `socket.error` (alias of OSError), `herror`, `socket.timeout` (we raise builtin `TimeoutError`). `setsockopt`/`getsockopt` are int-valued only — byte-buffer options aren't expressible without `bytes`-typed extern args. `gethostbyname` only returns the first IPv4 (no `gethostbyname_ex` triple). |
| `keyword` | `kwlist`, `softkwlist`, `iskeyword(s)` | None — full parity with CPython 3.13. |
| `errno` | All 106 `errno.h` integer constants on Darwin/BSD, `EWOULDBLOCK` alias, `errorcode` reverse-lookup map | Values are pinned to the Darwin/macOS build host; on Linux the integers differ (notably `EAGAIN=11`, `EINPROGRESS=115`). Names and `errorcode` semantics are identical. CPython annotates `errorcode` as `dict[int, str]`; spython uses `map[int, str]` for the same type. |
| `stat` | `S_IFMT` / `S_IMODE` functions, file-type constants, file-type predicates (`S_ISDIR`, `S_ISREG`, …), permission-bit constants, `filemode(mode)` | None — output matches CPython byte-for-byte across all common modes. |
| `colorsys` | `rgb_to_yiq` / `yiq_to_rgb` / `rgb_to_hls` / `hls_to_rgb` / `rgb_to_hsv` / `hsv_to_rgb` | None — coefficients match CPython exactly; hue normalization works around spython's C-style float `%`. |
| `itertools` | `count(start, step)`, `repeat(value, times)` / `repeat_forever(value)`, `cycle(xs)`, `chain(its)` / `chain_lists(xss)`, `islice(it, stop)` / `islice_range(it, start, stop, step)`, `accumulate(xs)`, `pairwise(xs)`, `compress(data, selectors)`, `batched(xs, n)`, `tee(it)`, `combinations(xs, r)`, `permutations(xs, r)` | All callable-arg variants (`filterfalse`, `dropwhile`, `takewhile`, `starmap`, `groupby`, `accumulate(func=...)`) — closures still missing. `chain(*iterables)` becomes `chain(its)` (varargs of generator params not yet allowed). Polymorphic iterators are int-specialized. `tee` returns two materialized lists rather than two lazy views. `product`, `zip_longest` need heterogeneous tuple typing. |

---

## Initially-promising modules that still turn out partial

These looked like clean candidates, but each has a default arg, callable
kwarg, generator return, or other CPython-specific shape we can't replicate
without compiler work.

| Module | Actual blocker |
|---|---|
| `cmath` | `log(z[, base])` optional; `isclose(a, b, *, rel_tol, abs_tol)` kwargs; returns native `complex` (we have no complex type). |
| `string` | `Template.substitute(**kwargs)` is now writable in shape, but the values must all share one type — CPython accepts arbitrary objects. `Formatter` mixes positional + keyword forwarding through `vformat(fmt, args, kwargs)` and would still need first-class callables for custom converters. Constants + `capwords` are fine. |
| `textwrap` | Every function takes ≥6 keyword args (`width=70, initial_indent='', …`). Same shape, no defaults. |
| `heapq` | `merge(*iterables, key=None, reverse=False)` — varargs are fine now, but defaults + callable kwarg + generator return still block; `nlargest` / `nsmallest(n, it, key=None)` defaults a callable. |
| `bisect` | `bisect_left(a, x, lo=0, hi=None, *, key=None)` — defaults + callable kwarg. |
| `glob` | `glob(pat, *, recursive=False, root_dir=None, dir_fd=None, include_hidden=False)` plus `iglob` is a generator. |
| `mimetypes` | Module-level functions are fine; `MimeTypes` class has init + `read` / `readfp` taking file-like objects. |
| `ipaddress` | `ip_address(addr)` accepts `int | str | bytes` (union types); constructors take many kwargs. |
| `uuid` | `UUID(hex=None, bytes=None, bytes_le=None, fields=None, int=None, version=None, *, is_safe=...)` — the alt-kwargs constructor pattern is now syntactically expressible, but every parameter has a default. Still blocked on defaults. |
| `secrets` | Every function has a default arg (`token_bytes(nbytes=None)`). |
| `hmac` | `new(key, msg=None, digestmod='')` — `digestmod` accepts a string OR a callable OR a module. |
| `calendar` | `Calendar(firstweekday=0)`, `month(theyear, themonth, w=0, l=0)`, `prmonth(theyear, …)` — defaults everywhere. |
| `getopt` | `getopt(args, shortopts, longopts=[])` — default. Otherwise close. |
| `mmap`, `select`, `signal`, `subprocess`, `shutil`, `sqlite3`, `ssl`, `zlib` / `gzip`, `datetime`, `pathlib`, `argparse`, `csv` | All have either default args, generator returns, callable kwargs, context manager use, or all of the above pervasively. |

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
5. **Default arguments** — unlocks the long tail: `dict.get(k, default)`,
   `print`'s `sep=` / `end=`, almost every `textwrap` / `heapq` / `bisect` /
   `glob` / `secrets` / `calendar` signature, and most CPython kwargs.
   Varargs / `**kwargs` / keyword-only params landed in this revision and
   are no longer blockers, but they only help where the values share a
   single type — heterogeneous variadics like `struct.pack(fmt, *vals)`
   still need richer typing.
6. **Dynamic typing / `Any`** — unlocks `pickle`, generic `copy.deepcopy`,
   schema-free `json`, and the heterogeneous-tuple side of `struct` /
   `string.Template`.

Generators (`yield`, `yield from`, `Iterator[T]`, `next()`,
`StopIteration`) shipped in the prior revision and a generator-based
`itertools` (count / repeat / cycle / chain / islice / accumulate /
pairwise / compress / batched / tee / combinations / permutations)
shipped this revision. Callable-arg variants (`filterfalse`, `dropwhile`,
`takewhile`, `starmap`, `groupby`, `accumulate(func=...)`) are still
blocked on closures; the iterator surface of `csv` / `io` / `os` / `xml`
/ `email` / `re.finditer` is also unblocked. Closures are now the
largest remaining lever.

---

## Honest framing

spython's stdlib is **name-compatible and purpose-compatible** with CPython,
not **signature-compatible** — until generators, defaults, and first-class
callables land. (Varargs / `**kwargs` / keyword-only params and call-site
keyword passing + `*` / `**` unpacking now work, so signatures that only
needed those features are newly reachable.) Documentation should say so
plainly so users don't expect `from math import isclose` or `os.walk(".")`
to work.
