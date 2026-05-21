# `requests` — spython vs CPython

This document records every known difference between
`stdlib/requests.spy` (spython) and CPython's `requests` library
(plus its `urllib3` / `http.client` underpinnings). It is meant as a
porting guide and as an honest accounting of the gap.

---

## At a glance

| Dimension | spython | CPython |
| --- | --- | --- |
| Lines of impl | ~530 (.spy) + ~50 (.c) | ~10k (`urllib3`) + ~3k (`requests`) |
| Module functions | get, post, put, delete, head, patch, options, request | same names + Session methods |
| Response attrs | url, status_code, reason, headers, content, text, encoding, ok | adds: cookies, history, elapsed, links, request, is_redirect, is_permanent_redirect, next, apparent_encoding |
| Response methods | json, raise_for_status | adds: iter_content, iter_lines, close, __enter__/__exit__ |
| Exceptions | RequestException, HTTPError, InvalidURL | 20+ subclasses |
| HTTP version | 1.0 | 1.1 |
| Keep-alive | never | default |
| Redirects | never followed | followed by default |
| Cookies | none | yes |
| Compression | none | gzip / deflate / br |
| Auth handlers | none | basic / digest / oauth / custom |
| Proxies | none | http / https / socks |
| Sessions | none | yes |
| Connection pooling | none | yes |

---

## Public API

### Module-level functions

Both expose the same eight HTTP-method helpers, but the signatures
differ.

**spython**

```python
import requests

headers: map[str, str] = {}
r = requests.get(url, headers)
r = requests.post(url, body, headers)
r = requests.put(url, body, headers)
r = requests.delete(url, headers)
r = requests.head(url, headers)
r = requests.patch(url, body, headers)
r = requests.options(url, headers)
r = requests.request(method, url, body, headers)
```

- `headers` is a **required positional** `map[str, str]`.
- `body` is a **required positional** `bytes` for verbs that carry one.
- No keyword arguments are accepted.

**CPython**

```python
import requests

r = requests.get(url, params=None, **kwargs)
r = requests.post(url, data=None, json=None, **kwargs)
# ... etc; kwargs include headers, cookies, files, auth, timeout,
# allow_redirects, proxies, hooks, stream, verify, cert
```

- All extra parameters are keyword arguments with defaults.
- `headers` accepts dict, `CaseInsensitiveDict`, or list-of-tuples.
- `data` accepts dict (form-encoded), bytes, str, or file-like.
- `json` accepts a Python value that gets `json.dumps`'d and gets
  `Content-Type: application/json` set automatically.
- `params` accepts a dict that gets URL-query-encoded.

### `Response` class

| Attribute / method | spython | CPython |
| --- | --- | --- |
| `url` | str | str (final URL after redirects) |
| `status_code` | int | int |
| `reason` | str | str |
| `headers` | `map[str, str]`, **lowercased keys** | `CaseInsensitiveDict` preserving original casing |
| `content` | bytes (whole body) | bytes |
| `text` | byte-identical `str(content)` | decoded with `encoding` (charset-detected) |
| `encoding` | always `"utf-8"` (sentinel only) | autodetected via `chardet` / `charset_normalizer`, settable |
| `ok` | True iff 200 ≤ status < 400 | same |
| `json()` | returns `Any` (must unbox via `any_dict` / `any_list` / `any_str` / ...) | returns native dict/list/etc. |
| `raise_for_status()` | raises `HTTPError` on ≥ 400 | same shape, different class |
| `cookies` | — | `RequestsCookieJar` |
| `history` | — | list of intermediate `Response`s |
| `elapsed` | — | `timedelta` |
| `links` | — | parsed `Link:` header |
| `request` | — | the `PreparedRequest` |
| `is_redirect` / `is_permanent_redirect` | — | bool |
| `next` | — | next `PreparedRequest` if redirect |
| `apparent_encoding` | — | charset detector's guess |
| `iter_content(chunk_size)` | — | streaming iterator |
| `iter_lines(...)` | — | streaming iterator |
| `close()` | — | release pooled connection |
| `__enter__` / `__exit__` | — | (spython has no `with` yet) |

### Exceptions

**spython** (only three classes):

```
Exception                   (built-in)
└── RequestException
    ├── HTTPError           — raised by raise_for_status() on 4xx / 5xx
    └── InvalidURL          — bad / unsupported scheme
```

Connection-level failures (DNS, connect, TLS handshake, broken pipe,
recv timeout) all bubble up as plain `RequestException` with a prose
message, **not** dedicated subclasses. The reason is that spython has
a flat class namespace and the auto-injected builtin exception
hierarchy already defines `ConnectionError`, `TimeoutError`,
`ConnectionRefusedError`, etc., which would collide with CPython's
`requests.exceptions.ConnectionError`/`Timeout`.

**CPython** has 20+ exception classes including:

```
RequestException
├── HTTPError
├── ConnectionError
│   ├── ConnectTimeout
│   ├── ReadTimeout
│   ├── ProxyError
│   ├── SSLError
│   └── ChunkedEncodingError
├── Timeout
│   ├── ConnectTimeout
│   └── ReadTimeout
├── URLRequired
├── TooManyRedirects
├── MissingSchema
├── InvalidSchema
├── InvalidURL
├── InvalidHeader
├── InvalidJSONError
├── ContentDecodingError
├── StreamConsumedError
├── RetryError
├── UnrewindableBodyError
└── FileModeWarning
```

### Module-level toggles

spython exposes two process-wide setters that CPython doesn't have
(CPython threads these through per-call kwargs):

| spython | CPython equivalent |
| --- | --- |
| `requests.set_verify(False)` | `verify=False` per call |
| `requests.set_timeout(seconds)` | `timeout=seconds` per call |

CPython's `timeout` accepts a `(connect, read)` tuple. spython's
single `set_timeout` value applies to DNS lookup, TCP connect, and
each individual `recv`/`send` syscall.

### Things that don't exist on the spython side

- `Session` (cookie/session persistence)
- `PreparedRequest`, `Request`
- `requests.adapters.HTTPAdapter` (so no per-host config / retries /
  pool size)
- `requests.auth.HTTPBasicAuth` etc.
- `requests.utils.*` helpers
- `requests.cookies.*`
- Hooks (`response` callable on every reply)

---

## Behavioural differences

### URL handling

| Aspect | spython | CPython |
| --- | --- | --- |
| Schemes | `http`, `https` only | `http`, `https`, `file`, custom adapters |
| Query-string building | none — caller pre-builds | `params={...}` autoencodes |
| URL percent-encoding | none — caller pre-encodes | done automatically |
| Userinfo (`user:pass@`) | not parsed | parsed and used for auth |
| IDN (non-ASCII hostnames) | not supported (no `idna`) | encoded via `idna` |
| IPv6 literals (`[::1]`) | not supported (`socket.c` is AF_INET only) | supported |

### Request line / headers

- Wire format is **HTTP/1.0**. Every request includes
  `Connection: close`. There is no keep-alive, no pipelining, and the
  TCP+TLS connection is torn down after every response.
- Default headers we add (only when not already in the user map):
  `Host`, `Connection: close`, `User-Agent: spython-requests/0.1`,
  `Accept: */*`, and `Content-Length:` when there's a body.
- We send no `Accept-Encoding`, so servers will not gzip the body.
- Headers are `map[str, str]`. Multi-valued headers collapse to the
  last value seen on the wire; CPython's `CaseInsensitiveDict`
  joins multiple values with `, `.

### Request body

| Input | spython | CPython |
| --- | --- | --- |
| bytes | passed through as-is | passed through as-is |
| str | not accepted (must `bytes(s)` first) | UTF-8 encoded, sets text Content-Type |
| dict (form) | not accepted | URL-form-encoded, sets `application/x-www-form-urlencoded` |
| dict (json) | not accepted | use `json=` kwarg → `json.dumps` + `application/json` |
| file-like | not accepted | streamed |
| `files=` multipart | not accepted | multipart/form-data |

### Response framing

We support exactly three framing strategies, in this order:

1. `Content-Length: N` → read exactly N bytes after the header
   terminator.
2. `Transfer-Encoding: chunked` → decode the chunked stream, stopping
   at the 0-size chunk (trailers are dropped).
3. Neither header → read until peer closes (the `Connection: close`
   we always send forces this on conformant HTTP/1.0 servers).

CPython delegates this to `http.client` and additionally handles
`Trailer:` headers, multiple `Transfer-Encoding` codings,
`Content-Encoding` decompression, and HTTP/1.1 keep-alive framing.

### Compression / content negotiation

- We do **not** send `Accept-Encoding`.
- If a server returns `Content-Encoding: gzip` (or `deflate` / `br`)
  anyway, the body in `r.content` will be the raw compressed bytes —
  there is no decoder.
- CPython negotiates `gzip`/`deflate` (and `br` with the optional
  `brotli` package) and decompresses transparently.

### Charset / text decoding

- `r.text` in spython is `str(r.content)` — a byte-identity copy with
  no charset interpretation. It works correctly for UTF-8 and ASCII.
- CPython runs `chardet` / `charset_normalizer` on the body, sets
  `r.encoding` to the guess, then decodes lazily.
- spython's `r.encoding` is always the literal `"utf-8"` and changing
  it has no effect.

### Redirects

- spython **never** follows redirects. A `301`/`302`/`307`/`308`
  shows up in `r.status_code`; the user must inspect
  `r.headers["location"]` and call `requests.get` again themselves.
- CPython follows up to 30 by default, populating `r.history` with
  intermediate responses. Redirect handling can be disabled with
  `allow_redirects=False`.

### Cookies

- spython has none. Set-Cookie headers are visible in
  `r.headers["set-cookie"]` (a single string; multiple Set-Cookies
  collapse), but nothing parses or persists them.
- CPython has `RequestsCookieJar`, per-Session cookie persistence,
  and per-domain isolation.

### Authentication

- spython has none. Basic auth means setting an `Authorization:
  Basic ...` header by hand (with `base64.b64encode` from stdlib).
- CPython provides `HTTPBasicAuth`, `HTTPDigestAuth`, `HTTPProxyAuth`,
  arbitrary auth callables, and `requests-oauthlib` integration.

### Proxies

- spython has none. There is no `HTTP_PROXY` env-var honouring, no
  `CONNECT`-tunnel for HTTPS-over-HTTP-proxy, no SOCKS.
- CPython supports per-call `proxies={...}` and reads
  `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` from the environment.

### TLS

| Aspect | spython | CPython |
| --- | --- | --- |
| Backend | OpenSSL via direct FFI in `stdlib/ssl.c` | `ssl` module + OpenSSL |
| Min version | TLS 1.2 (hardcoded) | TLS 1.2 (configurable per call) |
| Trust store | `SSL_CTX_set_default_verify_paths` (system) | bundled `certifi` PEM by default |
| Hostname check | `X509_VERIFY_PARAM_set1_host` (on) | matches hostname (on) |
| Verify off | `requests.set_verify(False)` (process-wide) | `verify=False` per call, or path to a CA bundle |
| Client certs | not supported | `cert=path` or `(certfile, keyfile)` |
| ALPN | not requested | requested when available |
| Session resumption | none | OpenSSL session cache |

### Timeouts

| Phase | spython | CPython |
| --- | --- | --- |
| DNS | bounded (`set_timeout`); `getaddrinfo` runs on a worker thread, polled with the deadline | typically system-resolver default; may hang |
| TCP connect | bounded (non-blocking + `select`) | `(connect_timeout, read_timeout)` tuple |
| TLS handshake | bounded by `SO_RCVTIMEO`/`SO_SNDTIMEO` on the underlying fd | bounded by connect timeout |
| recv / send | bounded by `SO_RCVTIMEO`/`SO_SNDTIMEO` | bounded by read timeout |
| On expiry | recv returns empty → caller exits with whatever was buffered (no exception) | raises `Timeout` / `ConnectTimeout` / `ReadTimeout` |

### Streaming

- spython always buffers the entire body into `r.content` before
  returning. There is no `stream=True`, `iter_content()`, or
  `iter_lines()`.
- CPython's `stream=True` returns the response with the body still
  on the wire; the caller drains it via the iterators.

### Connection pooling

- spython opens a fresh TCP+TLS connection for every request. There
  is no pool, no warm reuse, no keep-alive. A 100-request loop pays
  ~100 TLS handshakes.
- CPython (via `urllib3.PoolManager`) reuses connections within a
  `Session`; without a Session, `requests` still uses a
  thread-local default pool.

---

## Underlying stdlib differences

| CPython requests pulls in | spython substitute |
| --- | --- |
| `urllib3` (~10k LOC) | hand-rolled framing in `requests.spy` (~530 lines) |
| `urllib.parse` | minimal `_parse_url` in `requests.spy` |
| `http.client` | hand-rolled status-line + header parser |
| `email.parser` | not used; we tokenise headers byte-by-byte |
| `certifi` | system trust store (`SSL_CTX_set_default_verify_paths`) |
| `idna` | not present — ASCII hostnames only |
| `charset_normalizer` / `chardet` | not present — UTF-8 only |
| `gzip` / `zlib` / `brotli` | not present — no Content-Encoding decode |
| `cookielib` / `http.cookiejar` | not present |

---

## Limitations rooted in spython itself

These aren't really `requests` design choices — they fall out of
features the compiler doesn't yet support.

- **No keyword arguments on user-facing helpers.** `headers` and
  `body` are positional because spython has no `**kwargs` plumbing
  for user-defined functions in this surface. CPython's `verify=`,
  `timeout=`, `allow_redirects=`, etc. become `requests.set_verify()`
  / `requests.set_timeout()` module setters.
- **No nullable types.** Every helper that "doesn't take a body"
  (`get`, `delete`, `head`, `options`) still requires a `headers`
  argument; pass `{}` for an empty header set. You cannot pass
  `None`.
- **`Response.json()` returns `Any`.** spython has no dynamic
  `dict[str, object]`, so JSON values come back as the tagged-union
  `Any` introduced for `json.loads`. Walk it with `any_dict`,
  `any_list`, `any_str`, `any_int`, `any_float`, `any_bool`,
  `any_is_none`. CPython's `r.json()` returns native dicts/lists.
- **`Connection: close` is mandatory.** Without keep-alive support
  and without proper HTTP/1.1 framing, sending `Connection: close`
  is the only way to make body-end detection reliable when neither
  Content-Length nor Transfer-Encoding is present.
- **No `with` statement support.** `with requests.Session() as s:`
  has no analogue; you'd manually call a hypothetical `s.close()`.
- **No regex-based URL/header validation.** spython's `re` module
  exists but is POSIX ERE-only, missing `\d`/`\w`/`\s`, lookaround,
  and named groups, so we walk bytes by hand.

---

## Things that are intentionally identical

For the parts that *are* implemented, we tried to match CPython's
shape exactly so user code transcribes one-to-one:

- Method-name set: `get`, `post`, `put`, `delete`, `head`, `patch`,
  `options`, `request`.
- Response field names: `status_code`, `reason`, `headers`, `content`,
  `text`, `url`, `ok`, `encoding`.
- `Response.json()` parses the body as JSON.
- `Response.raise_for_status()` raises on `>= 400`.
- HTTP/`status` mapped to `ok` exactly the way CPython does
  (`200 <= code < 400`).
- Exception root class is named `RequestException`.
- Hostname verification is **on** by default; the only way to turn it
  off is the explicit verify-disable toggle.

If you find a behaviour difference not in this table, it is a bug —
please file it.
