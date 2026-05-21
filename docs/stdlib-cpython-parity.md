# CPython stdlib parity in spython

> **29 modules now ship.** Since the prior revision, the `Any` unbox surface
> (`any_dict` / `any_list` / `any_str` / `any_int` / `any_float` / `any_bool`
> / `any_is_none`) landed and unblocked a real **`json`** (`loads(s) -> Any`,
> `dumps(value: Any) -> str`); **`fnmatch`** landed in full (slicing-based
> matcher); and **`requests`** (HTTP/1.1 client over `socket` + `ssl`),
> **`ssl`** (`SSLContext` / `SSLSocket` / `create_default_context`),
> **`secrets`**, **`shutil`**, **`string`**, **`textwrap`**, and
> **`functools.reduce`** all shipped. The body tables and priority list below
> have been reconciled to this 29-module reality.

> **Closures shipped** (lambdas + nested `def`, captured by value, with the
> `Callable[[Args], Ret]` type) — the first item on the "headline blockers"
> list below. Concretely landed: the `itertools` callable variants
> `takewhile` / `dropwhile` / `filterfalse` / `starmap` (plus
> `accumulate_with`, the func-taking accumulate), and a new `functools`
> module with `reduce(function, sequence)`. Callable params can also carry
> lambda defaults (`key: Callable[...] = lambda x: x`), so `key=`-style
> signatures are expressible on *non-generator* functions — generators still
> reject default parameters, which is why `accumulate`'s `func=` default
> specifically can't ship (hence the separate `accumulate_with`). Still
> blocked even with closures: union-typed callable args like `re.sub`'s
> `str | Callable` repl, and varargs-capturing `functools.partial`.

> **An earlier revision shipped five new modules and widened several existing
> ones.** New: `fnmatch` (full 1:1 — the last outstanding candidate),
> `string` (constants + `capwords`), `textwrap` (`wrap`/`fill`/`shorten`/
> `dedent`/`indent`), `secrets` (`token_hex`/`token_bytes`/`token_urlsafe`/
> `randbelow`/`compare_digest`), and `shutil` (`copyfile`/`copy`/`copy2`/
> `move`/`rmtree`/`which`). Widened: `math` (+`comb`/`perm`/`isclose`/`prod`/
> `dist`/`remainder`/`nextafter`/`ulp`/`frexp`/`modf`), `random`
> (+`randrange` and the `gauss`/`normalvariate`/`lognormvariate`/
> `expovariate`/`paretovariate`/`weibullvariate`/`triangular` variates),
> `os` (+`getenv(key, default)`/`makedirs`/`urandom`/`system`/`strerror`),
> `os.path` (+`isabs`/`normpath`/`abspath`/`expanduser`/`commonprefix`/
> `getsize`/`getmtime`/`splitdrive`), `base64` (+`standard_*`/`urlsafe_*`/
> `b16encode`/`b16decode`), `binascii` (+`b2a_hex`/`a2b_hex`), `hashlib`
> (+`new(name, data)`), and `time` (+`struct_time`/`localtime`/`gmtime`/
> `mktime`/`strftime`/`asctime`/`ctime`). A compiler fix also enabled
> `==`/`!=`/`<`/`<=`/`>`/`>=` and `+` on `bytes`/`bytearray`.


An honest accounting of how spython's stdlib relates to CPython's. Names and
purposes match; **signatures generally do not**. This document captures the
gaps so that users (and we) don't mistake "module exists" for "drop-in
replacement."

## TL;DR

- **29 modules** ship. **5 of 29** match CPython's public API 1:1:
  `keyword`, `errno` (Darwin-pinned values), `stat`, `colorsys`, and
  `fnmatch` (which landed this revision — slicing-based matcher covering
  `fnmatch` / `fnmatchcase` / `filter`; `translate` is the one piece still
  out, since it would emit a Python-`re` regex the POSIX engine can't run).
  `itertools` ships as a generator-based subset; its
  callable-arg variants (`takewhile`/`dropwhile`/`filterfalse`/`starmap`/
  `accumulate_with`) now ship since closures landed (`groupby` still
  omitted). `re` shipped this
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
| `itertools` | `count(start, step)`, `repeat(value, times)` / `repeat_forever(value)`, `cycle(xs)`, `chain(its)` / `chain_lists(xss)`, `islice(it, stop)` / `islice_range(it, start, stop, step)`, `accumulate(xs)` / `accumulate_with(xs, func)`, `takewhile(pred, xs)`, `dropwhile(pred, xs)`, `filterfalse(pred, xs)`, `starmap(func, pairs)`, `pairwise(xs)`, `compress(data, selectors)`, `batched(xs, n)`, `tee(it)`, `combinations(xs, r)`, `permutations(xs, r)` | The callable-arg variants now ship (closures landed): `takewhile` / `dropwhile` / `filterfalse` / `starmap`, plus `accumulate_with` for the func-taking accumulate. Still missing: `groupby` (yields `(key, group)` — heterogeneous), and `accumulate`'s own `func=` default (generators can't take default params, so it's split out as `accumulate_with`). `chain(*iterables)` becomes `chain(its)` (varargs of generator params not yet allowed). Polymorphic iterators are int-specialized. `tee` returns two materialized lists rather than two lazy views. `product`, `zip_longest` need heterogeneous tuple typing. |
| `re` | `search` / `match` / `fullmatch` returning a `Match`, `findall`, `finditer`, `sub` / `subn`, `split`, `escape`, `IGNORECASE` / `MULTILINE` (and aliases `I` / `M`); `error` exception class; `Match.group(i=0)` / `start` / `end` / `span` / `groups()` / `.matched` / `.string`. Backed by libc's POSIX ERE engine (regcomp/regexec) — no extra link flags. Single-slot compile cache amortises repeated `_exec` calls inside findall/finditer/sub. | Engine is POSIX ERE, so the recognised pattern syntax is **not** Python re: no `\d` / `\w` / `\s` / `\b`, no `(?:...)` / `(?=...)` / `(?!...)` / `(?<...)`, no `(?P<name>...)` named groups, no inline `(?i)` flags, no in-pattern backreferences (POSIX BRE has them, ERE doesn't). `re.DOTALL` is accepted but is a noop — POSIX `.` matches `\n` by default and `MULTILINE` (= `REG_NEWLINE`) flips both `^/$` *and* dot-vs-newline in lockstep. `re.compile` / `re.Pattern` are not shipped (a single-slot cache covers the common loop case). `re.search` returns a Match with `.matched=False` instead of `None` (no Optional sugar yet). `findall` always returns the full-match text; CPython returns capture-group tuples when groups exist, but `list[str]` is homogeneous. Callable `repl` for `sub` is blocked on closures. Embedded NUL bytes truncate the search (POSIX regexec is null-terminated). |
| `heapq` | `heappush` / `heappop` / `heapify` / `heapreplace` / `heappushpop`, `nlargest(n, it, key=lambda x: x)` / `nsmallest(n, it, key=...)`, `merge(lists)`. Heap layout follows CPython's `_siftup`/`_siftdown` so pop order (incl. ties) matches. | Int-specialized (`list[int]` heaps). `merge` is `merge(lists)` not `merge(*iterables, key=, reverse=)` — generators can't take varargs or default params, so its `key=`/`reverse=` are dropped. CPython's `key=None` identity is spelled `key=lambda x: x` (a lambda default on the non-generator `nlargest`/`nsmallest`). `_heapify_max` / `merge`'s lazy N-way nature aside, the heap ops are 1:1. |
| `bisect` | `bisect_left` / `bisect_right` / `bisect`, `insort_left` / `insort_right` / `insort`, each `(a, x, lo=0, hi=-1, key=lambda v: v)`. As in CPython 3.10+, `key` applies to array elements, not to `x`. | Int-specialized. CPython's `hi=None` (→ len(a)) is modelled with the sentinel `hi=-1`. Otherwise signature- and behavior-compatible, including the `key=` argument. |
| `list.sort(key=, reverse=)` | The list method (not a module): `xs.sort(key=lambda e: ...)` and `xs.sort(reverse=True)`, including both together, with a stable sort. Lowered to a closure-aware runtime sort (`spy_list_sort_key`) that computes keys by invoking the closure, then stably sorts an index permutation. | Element type must be `int`/`float`/`str`; `key` must be a closure (wrap a named function in a lambda) returning `int`/`float`/`str`. The captured free vars in the key must be function locals (capturing a module-level global in a lambda is a standing closure-codegen limitation). The free `sorted(iterable, key=)` builtin is still not provided. |
| `json` | `loads(s) -> Any`, `dumps(value: Any) -> str`. The decoded `Any` tree is navigated with the `any_dict` / `any_list` / `any_str` / `any_int` / `any_float` / `any_bool` / `any_is_none` unbox builtins. | No `JSONDecoder` / `JSONEncoder` classes, no `load` / `dump` (file objects), none of the `dumps` kwargs (`indent`, `sort_keys`, `separators`, `default`, `ensure_ascii`, `cls`), no `object_hook` / `parse_float` hooks. CPython hands back live `dict`/`list` objects; spython hands back an `Any` you must explicitly unbox by type. `JSONDecodeError` is not raised. |
| `fnmatch` | `fnmatch(name, pat)`, `fnmatchcase(name, pat)`, `filter(names, pat)` — full glob semantics (`*`, `?`, `[seq]`, `[!seq]`) via a slicing-based matcher. | `translate(pat) -> str` is omitted: it would emit a Python-`re` flavored regex, which the POSIX-ERE `re` backend can't consume. CPython normalizes case via `os.path.normcase` on the platform; spython's `fnmatch` is case-sensitive (only `fnmatchcase`'s explicit contract). No regex caching layer (CPython memoizes `translate`). |
| `string` | Constants `ascii_lowercase` / `ascii_uppercase` / `ascii_letters` / `digits` / `hexdigits` / `octdigits` / `punctuation` / `whitespace` / `printable`; `capwords(s, sep='')`. | `Template` (`substitute` / `safe_substitute` — values must share one type in spython; CPython takes arbitrary objects), `Formatter` (needs first-class converters), `Formatter.vformat` / `parse` / `get_field`. Constants + `capwords` match CPython exactly. |
| `textwrap` | `wrap(text, width=70) -> list[str]`, `fill(text, width=70)`, `shorten(text, width, placeholder=' [...]')`, `dedent(text)`, `indent(text, prefix)`. | The remaining `TextWrapper` kwargs (`initial_indent`, `subsequent_indent`, `expand_tabs`, `replace_whitespace`, `drop_whitespace`, `break_long_words`, `break_on_hyphens`, `tabsize`, `max_lines`) and the `TextWrapper` class itself. `indent(text, prefix, predicate=)` drops the predicate callable. |
| `secrets` | `token_bytes(nbytes=32) -> bytes`, `token_hex(nbytes=32)`, `token_urlsafe(nbytes=32)`, `randbelow(n)`, `compare_digest(a, b)` (str). | `choice(seq)`, `randbits(k)`, the `SystemRandom` class. CPython's `nbytes=None` default (→ 32) is spelled `nbytes=32`. `compare_digest` is `str`-only (CPython also accepts `bytes`). |
| `shutil` | `copyfile(src, dst)` / `copyfile_into`, `copy(src, dst)`, `copy2(src, dst)`, `move(src, dst)`, `rmtree(path)`, `which(cmd)`. | `copytree`, `copyfileobj` / `copymode` / `copystat`, `make_archive` / `unpack_archive`, `disk_usage`, `chown`, the `ignore_patterns` / `dirs_exist_ok` / `follow_symlinks` / `onerror` kwargs. No file-object overloads. |
| `functools` | `reduce(function, sequence)` — two-arg form, `list[int]`-specialized. | 3-arg `reduce(f, it, initializer)` (needs a non-int sentinel), `partial` (varargs capture), `lru_cache` / `cache` / `cached_property` (decorators), `cmp_to_key`, `total_ordering`, `wraps` / `update_wrapper`, `singledispatch`, `reduce` over non-int element types. |
| `requests` | HTTP/1.1 client over `socket` + `ssl`: `get` / `post` / `put` / `delete` / `head` / `patch` / `options` / `request(method, url, body, headers)`; a `Response` (`.url` / `.status_code` / `.reason` / `.headers` / `.content` / `.text` / `.encoding` / `.ok` / `.json()` / `.raise_for_status()`); exceptions `RequestException` / `HTTPError` / `InvalidURL`; module config `set_verify(bool)` / `set_timeout(float)`. | `params=` / `json=` / `auth=` / `cookies=` / `stream=` / `allow_redirects=` / `proxies=` kwargs (`body: bytes` + `headers: map[str,str]` are positional, not keyword); the `Session` class; streaming / chunked iteration; redirect following; `Response.iter_content` / `iter_lines` / `.cookies` / `.history` / `.elapsed`. `headers` values are one type (`str`); a real CPython port reaches for richer mappings. See `docs/requests-vs-cpython.md`. |

---

## Initially-promising modules that still turn out partial

With defaults landed, several of these became newly reachable in shape —
the table below splits "newly reachable (additive work only)" from
"still blocked on a deeper feature."

### Newly reachable (defaults landed — pure additive port from CPython)

(`textwrap`, `secrets`, and `string` were in this bucket last revision and
have since **shipped** — see "Already shipped" above. The rest remain
reachable-but-unlanded.)

| Module | Notes |
|---|---|
| `uuid` | `UUID(hex=None, bytes=None, bytes_le=None, fields=None, int=None, version=None, *, is_safe=...)` is now syntactically expressible end-to-end. Class state + the `bytes` type still need work, but the signature is reachable. |
| `calendar` | `Calendar(firstweekday=0)`, `month(theyear, themonth, w=0, l=0)`, `prmonth(theyear, …)` — defaults were the blocker. |
| `getopt` | `getopt(args, shortopts, longopts=[])` — default was the blocker. |

### Still blocked on a deeper feature

| Module | Actual blocker |
|---|---|
| `cmath` | Returns native `complex` — we have no complex type. (`isclose` / `log(z[, base])` defaults are no longer a blocker.) |
| `string` (partial only) | The shipped subset (constants + `capwords`) is done; what's still blocked: `Template.substitute(**kwargs)` values must all share one type — CPython accepts arbitrary objects — and `Formatter` would need first-class callables for custom converters. |
| `glob` | `glob(pat, *, recursive=False, root_dir=None, dir_fd=None, include_hidden=False)` defaults + the `iglob` generator are no longer blockers. `fnmatch`-style matching now ships, so the matcher is available — the remaining work is the recursive `**` directory walk (and `iglob` as a generator). |
| `mimetypes` | Module-level functions are fine; `MimeTypes` class needs `read` / `readfp` taking file-like objects. |
| `ipaddress` | `ip_address(addr)` accepts `int \| str \| bytes` (union types); constructors take many kwargs. |
| `hmac` | `new(key, msg=None, digestmod='')` — `digestmod` accepts a string OR a callable OR a module (union + first-class callables). |
| `mmap`, `select`, `signal`, `subprocess`, `sqlite3`, `zlib` / `gzip`, `datetime`, `pathlib`, `argparse`, `csv` | Generator returns, callable kwargs, context manager use, or class-state-heavy APIs — all still pervasive even after defaults. (`shutil` and `ssl` were in this list last revision and have since shipped as subsets — see "Already shipped".) |

---

## Modules that genuinely can match CPython 1:1 today

All five original 1:1 candidates now ship (see "Already shipped" above):
`keyword`, `errno`, `stat`, `colorsys`, and `fnmatch`. `fnmatch` landed this
revision with a slicing-based matcher — `fnmatch` / `fnmatchcase` / `filter`
match CPython behavior; only `translate(pat)` is omitted (it would emit a
Python-`re` flavored regex the POSIX-ERE `re` backend can't run).

Landing the first four required relaxing the loader's
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

1. **Closures / function values as data** — **shipped.** Lambdas + nested
   `def` with by-value capture and the `Callable[[Args], Ret]` type, plus
   lambda defaults on non-generator params. Already cashed in:
   `itertools.takewhile` / `dropwhile` / `filterfalse` / `starmap` /
   `accumulate_with`, `functools.reduce`, `list.sort(key=, reverse=)`
   (codegen + runtime), and new `heapq` / `bisect` modules (with `key=`).
   Still to harvest: `defaultdict` (callable factory + class state),
   threading / signal callbacks, and a free `sorted(it, key=)` builtin. Not
   unlocked by closures alone: `re.sub`'s callable repl (union type
   `str | Callable`) and `functools.partial` (varargs capture).
2. **Decorators** — unlocks `dataclasses`, `functools.cache`, `@property`,
   `unittest` skip markers, much of `logging` configuration ergonomics.
3. **`with` / context managers** — ergonomic, not API-blocking; underlying
   classes work, callers just write `try` / `finally`.
4. **Metaclasses / runtime class creation** — unlocks `enum`, `abc`,
   `namedtuple`, ORM-style libs.
5. **Dynamic typing / `Any`** — a partial `Any` (with the `any_*` unbox
   builtins) shipped and already powers `json.loads` / `dumps`. A *fuller*
   `Any` would unlock `pickle`, generic `copy.deepcopy`, the live-`dict`
   ergonomics of `json` (vs. explicit unboxing today), and the
   heterogeneous-tuple side of `struct` / `string.Template`.

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
**Closures shipped this revision** (lambdas + nested `def`, by-value
capture, `Callable[[Args], Ret]` type), so the callable-arg `itertools`
variants (`takewhile`, `dropwhile`, `filterfalse`, `starmap`,
`accumulate_with`) and `functools.reduce` now ship; the iterator surface
of `csv` / `io` / `os` / `xml` / `email` / `re.finditer` is also
unblocked. What closures do *not* by themselves unlock: union-typed
callable args (`re.sub`'s `str | Callable` repl), varargs-capturing
`functools.partial`, default `func=`/`key=` on *generators* (generators
reject default params — `accumulate(func=...)` is split out as
`accumulate_with`), and decorators (`functools.cache` / `lru_cache`).

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
| 2 | **`json` — JSON encode/decode** | **Shipped this revision.** | `loads(s) -> Any` / `dumps(value: Any) -> str` ship, backed by the `Any` unbox builtins (`any_dict` / `any_list` / `any_str` / `any_int` / `any_float` / `any_bool` / `any_is_none`). The CPython "returns a live `dict`" ergonomics differ — you unbox the `Any` tree by type. Still missing: `dumps` kwargs (`indent` / `sort_keys` / `default` / …), `load` / `dump` file forms, hooks, and `JSONDecodeError`. |
| 3 | **`datetime`** | Blocked. | Needs class methods, ample defaults (now landed), and `timedelta` arithmetic via `__add__` / `__sub__` (operator overloads on user classes — check whether shipped). The data shape itself is fine. Should be reachable in stages. |
| 4 | **`pathlib.Path`** | Blocked. | Operator overload (`/`), method chaining, and `__fspath__` protocol. The path operations themselves all exist in `ospath`. Largely a rewrap. |
| 5 | **`argparse`** | Blocked. | Heterogeneous argument values (`type=int`, `action=...`), callable kwargs, and dynamic attribute access on `Namespace`. Needs closures + `Any`. A typed-builder variant could ship sooner. |
| 6 | **`logging`** | Blocked. | Class hierarchy + global config + handler callbacks. Needs closures for handlers and decorators for ergonomic use. A barebones `log.info(s)` / `log.error(s)` module-level shim is reachable today. |
| 7 | **`collections.defaultdict` / `Counter` / `OrderedDict` / `deque`** | Not shipped. | `defaultdict` needs a callable factory (closures). `Counter` and `OrderedDict` are reachable today as plain classes with fixed-type values. `deque` is a straight port. |
| 8 | **`io` — iteration + `with`** | Partially shipped (`open` returning `File`). | `for line in f:` needs the iterator protocol on `File`; `with f:` needs context managers. Both are real ergonomic blockers. |

### Tier 2 — heavy-use convenience (programs are noticeably worse without them)

| Rank | Need | Status | Gating work |
|---|---|---|---|
| 9 | **`functools.partial` / `lru_cache` / `reduce`** | **`reduce` shipped** (closures landed); `partial` / `lru_cache` still blocked. | `reduce(function, sequence)` ships in its two-arg form. `partial` needs varargs capture (heterogeneous); `lru_cache` needs decorators. The 3-arg `reduce(f, it, initializer)` needs a non-int sentinel for the optional initializer. |
| 10 | **`itertools` callable variants** (`filterfalse`, `dropwhile`, `takewhile`, `starmap`, `groupby`) | **Mostly shipped** (closures landed). | `takewhile` / `dropwhile` / `filterfalse` / `starmap` / `accumulate_with` all ship. Still missing: `groupby` (heterogeneous `(key, group)` output). |
| 11 | **`csv`** | Blocked. | `csv.reader` is a generator over rows (reachable now); `csv.DictReader` needs `Any`-typed values. The reader form is a clean shipping target. |
| 12 | **`subprocess.run` / `Popen`** | Blocked. | Heterogeneous kwargs (`stdin=...`, `capture_output=True`, `text=True`), context manager use, and the `bytes` type. Defaults landed; bytes + class state are the remainder. |
| 13 | **`shutil.copy*` / `rmtree` / `which`** | **Shipped this revision.** | `copyfile` / `copy` / `copy2` / `move` / `rmtree` / `which` ship. Still missing: `copytree`, `copyfileobj` / `copymode` / `copystat`, archive helpers, `disk_usage`, and the `ignore` / `dirs_exist_ok` / `onerror` kwargs. |
| 14 | **`enum`** | Blocked. | Metaclass-driven. No path without metaclasses. A `const`-style alternative could ship instead. |
| 15 | **`dataclasses`** | Blocked. | Decorator-driven `__init__` synthesis. No path without decorators. |
| 16 | **`typing` (compile-time hints)** | Mostly N/A — spython types are checked statically already. `Optional[T]`, `list[T]`, `dict[K,V]` already work. Runtime introspection (`get_type_hints`, `Protocol` checks) is permanently out of reach without runtime type objects. |

### Tier 3 — specialized but valuable

| Rank | Need | Status | Gating work |
|---|---|---|---|
| 17 | **`urllib.request` / `http.client` / `requests`** | **`requests` shipped this revision; `urllib`/`http.client` not.** | A `requests`-shaped HTTP/1.1 client now ships over `socket` + `ssl` (`get`/`post`/…, a `Response`, `set_verify`/`set_timeout`; see "Already shipped" and `docs/requests-vs-cpython.md`). `ssl` (`SSLContext`/`SSLSocket`) shipped too, so HTTPS works. The CPython `urllib.request` / `http.client` class APIs themselves are still unshipped (different surface); `requests` covers the common need. |
| 18 | **`unittest`** | Blocked. | Decorators (`@skip`), introspection (test discovery), and class-method dispatch for `setUp` / `tearDown`. |
| 19 | **`pickle` / `shelve`** | Permanently blocked without `Any`. | Wire format is dynamically typed by definition. |
| 20 | **`asyncio`** | Permanently blocked without `async`/`await` + closures. | Whole-language feature, not a stdlib question. |
| 21 | **`threading.Lock` / `Thread`** | Not shipped. | Threads themselves need a runtime model decision; `Lock` would need a target callable for the thread body (closures). |
| 22 | **`xml.etree`, `email.parser`, `html.parser`** | Not shipped. | Tree types are heterogeneous (`Any`). Iterator surfaces are reachable; trees are not. |

### What to ship next, opinionated

If the goal is **maximum programmer-impact per unit of work**, the order is:

1. ~~**`shutil` thin wrapper**~~ — **shipped** (`copy*` / `move` / `rmtree` / `which`).
2. **`Counter` / `OrderedDict` / `deque` in `collections`** — straight ports.
3. **Iterator protocol on `File`** (`for line in f:`) — small compiler change, large ergonomic win.
4. **Closures** — **shipped.** Already cashed in for the `itertools`
   callable variants, `functools.reduce`, `list.sort(key=, reverse=)`, and
   the new `heapq` / `bisect` modules. Remaining harvest: `defaultdict`
   (callable factory) and a free `sorted(it, key=)` builtin. Two related
   codegen items surfaced while landing this: (a) `for x in <list>`
   miscompiles *inside a generator* ("Instruction does not dominate all
   uses" — the indexed `while` walk is the workaround the stdlib already
   uses); (b) generators reject default parameters, so generator
   `func=`/`key=` defaults aren't expressible. Both are worth fixing to
   reach fuller parity.
5. **`with` statement desugar** — sugar over `try` / `finally`; modest compiler work, big readability gain.
6. **Decorators** — last of the big four, unlocks `dataclasses` / `lru_cache` / `unittest` markers.

Everything past that (metaclasses, `Any`, `async`) is a much bigger
language commitment and should follow user demand, not the parity
ranking.
