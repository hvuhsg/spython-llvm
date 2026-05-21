// stdlib/json.c — JSON parser and serialiser for stdlib/json.spy.
//
// loads(s) returns a SpyAny tagged value; dumps(any) returns a spython
// str. Strings allocated through spy_str_new are GC-rooted, the temporary
// buffer used during dumps is plain malloc'd and freed before return.
//
// Limits and behaviour:
//   * Numeric values without a decimal point or exponent become Any[int];
//     anything else becomes Any[float]. There is no big-integer fallback —
//     overflow silently wraps modulo 2^63 (this matches CPython 3 behavior
//     for numeric types stored in SpyAny, which is i64).
//   * Strings are parsed with the standard JSON escape set (\" \\ \/ \b
//     \f \n \r \t \uXXXX). Surrogate pairs are decoded into UTF-8.
//   * Object key order is not preserved — the underlying SpyMap is a
//     hash table.
//   * Errors print "JSONDecodeError: <message>" to stderr and abort(1).
//     A future revision wires this through the spython exception
//     system; the call shape doesn't change.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <math.h>
#include "runtime.h"

#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

// ===== Parser =====

typedef struct {
    const char *data;
    int64_t len;
    int64_t pos;
} JsonParser;

static void json_die(JsonParser *p, const char *msg) {
    fprintf(stderr, "JSONDecodeError: %s at position %lld\n",
            msg, (long long)p->pos);
    exit(1);
}

static void skip_ws(JsonParser *p) {
    while (p->pos < p->len) {
        char c = p->data[p->pos];
        if (c == ' ' || c == '\t' || c == '\n' || c == '\r') p->pos++;
        else break;
    }
}

static int peek(JsonParser *p) {
    if (p->pos >= p->len) return -1;
    return (unsigned char)p->data[p->pos];
}

static void expect_lit(JsonParser *p, const char *lit) {
    int64_t n = (int64_t)strlen(lit);
    if (p->pos + n > p->len || memcmp(p->data + p->pos, lit, n) != 0) {
        json_die(p, "expected literal");
    }
    p->pos += n;
}

// ----- string -----

// Append the UTF-8 encoding of codepoint cp to (buf,len,cap), growing as
// needed. cp may be up to 0x10FFFF.
static void utf8_append(char **buf, int64_t *len, int64_t *cap, uint32_t cp) {
    int n;
    char tmp[4];
    if (cp < 0x80) {
        n = 1; tmp[0] = (char)cp;
    } else if (cp < 0x800) {
        n = 2;
        tmp[0] = (char)(0xc0 | (cp >> 6));
        tmp[1] = (char)(0x80 | (cp & 0x3f));
    } else if (cp < 0x10000) {
        n = 3;
        tmp[0] = (char)(0xe0 | (cp >> 12));
        tmp[1] = (char)(0x80 | ((cp >> 6) & 0x3f));
        tmp[2] = (char)(0x80 | (cp & 0x3f));
    } else {
        n = 4;
        tmp[0] = (char)(0xf0 | (cp >> 18));
        tmp[1] = (char)(0x80 | ((cp >> 12) & 0x3f));
        tmp[2] = (char)(0x80 | ((cp >> 6) & 0x3f));
        tmp[3] = (char)(0x80 | (cp & 0x3f));
    }
    if (*len + n > *cap) {
        while (*len + n > *cap) *cap *= 2;
        *buf = (char*)realloc(*buf, (size_t)*cap);
    }
    memcpy(*buf + *len, tmp, (size_t)n);
    *len += n;
}

static int hex_digit(int c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static uint32_t parse_hex4(JsonParser *p) {
    if (p->pos + 4 > p->len) json_die(p, "bad \\u escape");
    uint32_t v = 0;
    for (int i = 0; i < 4; i++) {
        int d = hex_digit((unsigned char)p->data[p->pos + i]);
        if (d < 0) json_die(p, "bad \\u escape");
        v = (v << 4) | (uint32_t)d;
    }
    p->pos += 4;
    return v;
}

// Parse a JSON string starting at the opening quote. Returns a spython
// str (length-prefixed) containing the UTF-8 decoded value.
static char *parse_string_raw(JsonParser *p) {
    if (peek(p) != '"') json_die(p, "expected string");
    p->pos++;
    int64_t cap = 32;
    int64_t len = 0;
    char *buf = (char*)malloc((size_t)cap);
    while (1) {
        if (p->pos >= p->len) json_die(p, "unterminated string");
        unsigned char c = (unsigned char)p->data[p->pos++];
        if (c == '"') break;
        if (c == '\\') {
            if (p->pos >= p->len) json_die(p, "bad escape");
            unsigned char e = (unsigned char)p->data[p->pos++];
            uint32_t cp;
            switch (e) {
                case '"':  cp = '"'; break;
                case '\\': cp = '\\'; break;
                case '/':  cp = '/'; break;
                case 'b':  cp = '\b'; break;
                case 'f':  cp = '\f'; break;
                case 'n':  cp = '\n'; break;
                case 'r':  cp = '\r'; break;
                case 't':  cp = '\t'; break;
                case 'u': {
                    cp = parse_hex4(p);
                    if (cp >= 0xD800 && cp <= 0xDBFF) {
                        // High surrogate; expect \uLLLL low surrogate.
                        if (p->pos + 2 <= p->len
                            && p->data[p->pos] == '\\'
                            && p->data[p->pos + 1] == 'u') {
                            p->pos += 2;
                            uint32_t lo = parse_hex4(p);
                            if (lo >= 0xDC00 && lo <= 0xDFFF) {
                                cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00);
                            } else {
                                json_die(p, "bad surrogate");
                            }
                        } else {
                            json_die(p, "lone surrogate");
                        }
                    }
                    break;
                }
                default:
                    json_die(p, "bad escape");
                    return NULL; // unreachable
            }
            utf8_append(&buf, &len, &cap, cp);
        } else if (c < 0x20) {
            json_die(p, "control character in string");
        } else {
            // Pass through bytes verbatim (the input is assumed valid UTF-8).
            if (len + 1 > cap) {
                cap *= 2;
                buf = (char*)realloc(buf, (size_t)cap);
            }
            buf[len++] = (char)c;
        }
    }
    char *out = spy_str_new(buf, len);
    free(buf);
    return out;
}

static char *parse_value(JsonParser *p);

static char *parse_number(JsonParser *p) {
    int64_t start = p->pos;
    int is_float = 0;
    if (peek(p) == '-') p->pos++;
    if (peek(p) == '0') {
        p->pos++;
    } else if (peek(p) >= '1' && peek(p) <= '9') {
        while (peek(p) >= '0' && peek(p) <= '9') p->pos++;
    } else {
        json_die(p, "expected number");
    }
    if (peek(p) == '.') {
        is_float = 1;
        p->pos++;
        if (!(peek(p) >= '0' && peek(p) <= '9')) json_die(p, "bad number");
        while (peek(p) >= '0' && peek(p) <= '9') p->pos++;
    }
    if (peek(p) == 'e' || peek(p) == 'E') {
        is_float = 1;
        p->pos++;
        if (peek(p) == '+' || peek(p) == '-') p->pos++;
        if (!(peek(p) >= '0' && peek(p) <= '9')) json_die(p, "bad exponent");
        while (peek(p) >= '0' && peek(p) <= '9') p->pos++;
    }
    int64_t n = p->pos - start;
    char tmp[64];
    if (n >= (int64_t)sizeof(tmp)) {
        // Numeric token longer than 63 bytes — treat as float via strtod
        // (which can handle arbitrary length) but copy onto the heap.
        char *big = (char*)malloc((size_t)(n + 1));
        memcpy(big, p->data + start, (size_t)n);
        big[n] = 0;
        double d = strtod(big, NULL);
        free(big);
        return spy_any_box_float(d);
    }
    memcpy(tmp, p->data + start, (size_t)n);
    tmp[n] = 0;
    if (is_float) {
        return spy_any_box_float(strtod(tmp, NULL));
    }
    return spy_any_box_int((int64_t)strtoll(tmp, NULL, 10));
}

static char *parse_array(JsonParser *p) {
    if (peek(p) != '[') json_die(p, "expected '['");
    p->pos++;
    skip_ws(p);
    char *list = spy_list_new(8);
    if (peek(p) == ']') { p->pos++; return spy_any_box_list(list); }
    while (1) {
        skip_ws(p);
        char *v = parse_value(p);
        spy_list_append(list, (const char*)&v);
        skip_ws(p);
        int c = peek(p);
        if (c == ',') { p->pos++; continue; }
        if (c == ']') { p->pos++; break; }
        json_die(p, "expected ',' or ']' in array");
    }
    return spy_any_box_list(list);
}

static char *parse_object(JsonParser *p) {
    if (peek(p) != '{') json_die(p, "expected '{'");
    p->pos++;
    skip_ws(p);
    char *map = spy_map_new(8, 8, 1);
    if (peek(p) == '}') { p->pos++; return spy_any_box_map(map); }
    while (1) {
        skip_ws(p);
        char *key = parse_string_raw(p);
        skip_ws(p);
        if (peek(p) != ':') json_die(p, "expected ':' after object key");
        p->pos++;
        skip_ws(p);
        char *val = parse_value(p);
        spy_map_set(map, (const char*)&key, (const char*)&val);
        skip_ws(p);
        int c = peek(p);
        if (c == ',') { p->pos++; continue; }
        if (c == '}') { p->pos++; break; }
        json_die(p, "expected ',' or '}' in object");
    }
    return spy_any_box_map(map);
}

static char *parse_value(JsonParser *p) {
    skip_ws(p);
    int c = peek(p);
    switch (c) {
        case '{': return parse_object(p);
        case '[': return parse_array(p);
        case '"': return spy_any_box_str(parse_string_raw(p));
        case 't': expect_lit(p, "true");  return spy_any_box_bool(1);
        case 'f': expect_lit(p, "false"); return spy_any_box_bool(0);
        case 'n': expect_lit(p, "null");  return spy_any_none();
    }
    if (c == '-' || (c >= '0' && c <= '9')) return parse_number(p);
    json_die(p, "unexpected character");
    return NULL;
}

char *spy_json_loads(const char *s) {
    JsonParser p;
    p.data = SPY_STR_DATA(s);
    p.len = spy_str_len(s);
    p.pos = 0;
    skip_ws(&p);
    char *v = parse_value(&p);
    skip_ws(&p);
    if (p.pos != p.len) {
        fprintf(stderr, "JSONDecodeError: trailing data at position %lld\n",
                (long long)p.pos);
        exit(1);
    }
    return v;
}

// ===== Serialiser =====

typedef struct {
    char *buf;
    int64_t len;
    int64_t cap;
} JsonBuf;

static void buf_init(JsonBuf *b) {
    b->cap = 64;
    b->len = 0;
    b->buf = (char*)malloc((size_t)b->cap);
}

static void buf_grow(JsonBuf *b, int64_t need) {
    if (b->len + need <= b->cap) return;
    while (b->len + need > b->cap) b->cap *= 2;
    b->buf = (char*)realloc(b->buf, (size_t)b->cap);
}

static void buf_putc(JsonBuf *b, char c) {
    buf_grow(b, 1);
    b->buf[b->len++] = c;
}

static void buf_put(JsonBuf *b, const char *data, int64_t n) {
    buf_grow(b, n);
    memcpy(b->buf + b->len, data, (size_t)n);
    b->len += n;
}

static void buf_putstr(JsonBuf *b, const char *s) {
    buf_put(b, s, (int64_t)strlen(s));
}

// Write a spython str (length-prefixed) as a JSON string, with escapes.
// Bytes <0x20, '"' and '\\' get escaped; everything else is passed
// through (the input is assumed to be valid UTF-8).
static void write_string(JsonBuf *b, const char *spy_str) {
    int64_t n = spy_str_len(spy_str);
    const unsigned char *d = (const unsigned char*)SPY_STR_DATA(spy_str);
    buf_putc(b, '"');
    for (int64_t i = 0; i < n; i++) {
        unsigned char c = d[i];
        switch (c) {
            case '"':  buf_put(b, "\\\"", 2); break;
            case '\\': buf_put(b, "\\\\", 2); break;
            case '\b': buf_put(b, "\\b", 2); break;
            case '\f': buf_put(b, "\\f", 2); break;
            case '\n': buf_put(b, "\\n", 2); break;
            case '\r': buf_put(b, "\\r", 2); break;
            case '\t': buf_put(b, "\\t", 2); break;
            default:
                if (c < 0x20) {
                    char esc[8];
                    int m = snprintf(esc, sizeof(esc), "\\u%04x", c);
                    buf_put(b, esc, m);
                } else {
                    buf_putc(b, (char)c);
                }
        }
    }
    buf_putc(b, '"');
}

static void write_value(JsonBuf *b, const char *any);

static void write_list(JsonBuf *b, const char *list) {
    int64_t n = spy_list_len(list);
    buf_putc(b, '[');
    for (int64_t i = 0; i < n; i++) {
        if (i > 0) buf_putc(b, ',');
        char *slot = spy_list_get(list, i);
        char *any = *(char**)slot;
        write_value(b, any);
    }
    buf_putc(b, ']');
}

static void write_dict(JsonBuf *b, const char *map) {
    buf_putc(b, '{');
    int first = 1;
    int64_t idx = -1;
    while ((idx = spy_map_next(map, idx)) >= 0) {
        if (!first) buf_putc(b, ',');
        first = 0;
        char *key_slot = spy_map_key_at(map, idx);
        char *key = *(char**)key_slot;
        write_string(b, key);
        buf_putc(b, ':');
        char *val_slot = spy_map_val_at(map, idx);
        char *val = *(char**)val_slot;
        write_value(b, val);
    }
    buf_putc(b, '}');
}

static void write_value(JsonBuf *b, const char *any) {
    int tag = spy_any_tag(any);
    switch (tag) {
        case SPY_ANY_NONE:
            buf_putstr(b, "null");
            return;
        case SPY_ANY_BOOL:
            buf_putstr(b, spy_any_unbox_bool(any) ? "true" : "false");
            return;
        case SPY_ANY_INT: {
            char tmp[32];
            int n = snprintf(tmp, sizeof(tmp), "%lld",
                             (long long)spy_any_unbox_int(any));
            buf_put(b, tmp, n);
            return;
        }
        case SPY_ANY_FLOAT: {
            double v = spy_any_unbox_float(any);
            if (!isfinite(v)) {
                // JSON doesn't allow NaN/Inf; encode as null to keep
                // round-trips total. (CPython's json raises ValueError
                // by default but emits "NaN"/"Infinity" with allow_nan;
                // we avoid both choices here to keep the surface tiny.)
                buf_putstr(b, "null");
                return;
            }
            char tmp[64];
            // Round-trippable representation.
            int n = snprintf(tmp, sizeof(tmp), "%.17g", v);
            // If the result has neither '.' nor 'e', append ".0" so the
            // value stays a JSON number literal that round-trips back to
            // float (otherwise loads would re-parse it as int).
            int has_marker = 0;
            for (int i = 0; i < n; i++) {
                if (tmp[i] == '.' || tmp[i] == 'e' || tmp[i] == 'E') {
                    has_marker = 1;
                    break;
                }
            }
            buf_put(b, tmp, n);
            if (!has_marker) buf_put(b, ".0", 2);
            return;
        }
        case SPY_ANY_STR:
            write_string(b, spy_any_unbox_str(any));
            return;
        case SPY_ANY_LIST:
            write_list(b, spy_any_unbox_list(any));
            return;
        case SPY_ANY_DICT:
            write_dict(b, spy_any_unbox_map(any));
            return;
        case SPY_ANY_BYTES:
            // No defined JSON encoding for bytes; treat like null with a
            // warning to stderr so it's noisy but not fatal.
            fprintf(stderr, "json.dumps: bytes not supported, emitting null\n");
            buf_putstr(b, "null");
            return;
    }
    fprintf(stderr, "json.dumps: unknown Any tag %d\n", tag);
    buf_putstr(b, "null");
}

char *spy_json_dumps(const char *any) {
    JsonBuf b;
    buf_init(&b);
    write_value(&b, any);
    char *out = spy_str_new(b.buf, b.len);
    free(b.buf);
    return out;
}
