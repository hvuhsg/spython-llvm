#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <setjmp.h>
#include <math.h>
#include <gc.h>
#include "runtime.h"

// Render `x` into `buf` using a shortest round-trip representation,
// matching CPython's repr/str for floats. `buflen` must be at least 32.
// Returns the number of bytes written (excluding the null terminator).
// Non-finite values render as "nan", "inf", "-inf" with no ".0" suffix.
//
// Picks fixed-point form when 1e-4 <= |x| < 1e16, otherwise exponential —
// the cutoffs CPython's float_repr uses. Then chooses the shortest precision
// that strtod round-trips back to the original double, and strips trailing
// zeros so e.g. 0.5 prints as "0.5" not "0.50000".
static int spy_format_float(double x, char *buf, size_t buflen) {
    if (isnan(x)) return snprintf(buf, buflen, "nan");
    if (isinf(x)) return snprintf(buf, buflen, x < 0 ? "-inf" : "inf");
    if (x == 0.0) {
        return snprintf(buf, buflen, signbit(x) ? "-0.0" : "0.0");
    }

    // Find smallest precision P (1..17) for which %.*g round-trips.
    int precision = 17;
    char probe[64];
    for (int p = 1; p <= 17; p++) {
        snprintf(probe, sizeof(probe), "%.*g", p, x);
        if (strtod(probe, NULL) == x) { precision = p; break; }
    }

    double ax = fabs(x);
    int len;
    if (ax >= 1e-4 && ax < 1e16) {
        // Fixed form: render with %.*f using enough fractional digits to keep
        // `precision` significant digits, then strip trailing zeros.
        int exp10 = (int)floor(log10(ax));
        int frac = precision - 1 - exp10;
        if (frac < 0) frac = 0;
        len = snprintf(buf, buflen, "%.*f", frac, x);
        char *dot = strchr(buf, '.');
        if (dot) {
            char *end = buf + len - 1;
            while (end > dot + 1 && *end == '0') {
                *end-- = '\0';
                len--;
            }
        }
    } else {
        // Exponential form: %.*e then strip trailing zeros from the mantissa.
        len = snprintf(buf, buflen, "%.*e", precision - 1, x);
        char *e = strchr(buf, 'e');
        if (e) {
            char *end = e - 1;
            while (end > buf && *end == '0') end--;
            if (*end == '.') end--;
            // Shift the "e+NN" suffix down to right after the trimmed mantissa.
            size_t suffix_len = strlen(e);
            memmove(end + 1, e, suffix_len + 1);
            len = (int)strlen(buf);
        }
    }

    // Ensure ".0" suffix when there's no decimal or exponent marker so that
    // an integer-valued float like 10.0 doesn't print as "10".
    int has_marker = 0;
    for (int i = 0; i < len; i++) {
        if (buf[i] == '.' || buf[i] == 'e' || buf[i] == 'E') {
            has_marker = 1;
            break;
        }
    }
    if (!has_marker && (size_t)(len + 3) <= buflen) {
        buf[len++] = '.';
        buf[len++] = '0';
        buf[len] = '\0';
    }
    return len;
}

// ==================== Initialization ====================

void spy_init(void) {
    GC_INIT();
}

// Process argv stash for sys.argv. Codegen injects a call to spy_argv_set at
// the top of main() so any later user of sys.argv sees the values.
static int     spy_argc_val = 0;
static char  **spy_argv_val = NULL;

void spy_argv_set(int argc, char **argv) {
    spy_argc_val = argc;
    spy_argv_val = argv;
}

int spy_argv_count(void) { return spy_argc_val; }
const char *spy_argv_at(int i) {
    if (i < 0 || i >= spy_argc_val || spy_argv_val == NULL) return "";
    return spy_argv_val[i];
}

// ==================== Exception handling ====================
// Single-threaded model. If we ever add threads, move these to _Thread_local
// and call GC_add_roots per-thread for the in-flight pointer.

_Static_assert(sizeof(SpyExcFrame) <= 256,
    "SpyExcFrame must fit in the 256-byte alloca codegen emits per try");

static SpyExcFrame *spy_exc_top = NULL;
static void        *spy_exc_inflight = NULL;
static int          spy_exc_roots_registered = 0;

static void spy_exc_register_root(void) {
    if (!spy_exc_roots_registered) {
        GC_add_roots(&spy_exc_inflight, (char*)(&spy_exc_inflight) + sizeof(spy_exc_inflight));
        spy_exc_roots_registered = 1;
    }
}

void spy_exc_push(void *frame) {
    spy_exc_register_root();
    SpyExcFrame *f = (SpyExcFrame *)frame;
    f->prev = spy_exc_top;
    spy_exc_top = f;
    // buf is populated by the caller's setjmp after this returns.
}

void spy_exc_pop(void) {
    if (spy_exc_top) {
        spy_exc_top = spy_exc_top->prev;
    }
}

void *spy_exc_current(void) { return spy_exc_inflight; }
void  spy_exc_clear(void)   { spy_exc_inflight = NULL; }

void spy_exc_throw(void *obj) {
    spy_exc_inflight = obj;
    if (!spy_exc_top) {
        fprintf(stderr, "Uncaught exception\n");
        abort();
    }
    longjmp(spy_exc_top->buf, 1);
}

void spy_exc_rethrow(void) {
    if (!spy_exc_top) {
        fprintf(stderr, "Uncaught exception\n");
        abort();
    }
    longjmp(spy_exc_top->buf, 1);
}

// ==================== Print ====================

void spy_print_int(int64_t x) {
    printf("%lld", (long long)x);
}

void spy_print_float(double x) {
    char buf[64];
    spy_format_float(x, buf, sizeof(buf));
    fputs(buf, stdout);
}

void spy_print_bool(int x) {
    printf("%s", x ? "True" : "False");
}

void spy_print_str(const char *s) {
    if (s == NULL) return;
    // s points to a spy_str: first 8 bytes = len, then data
    int64_t len = *(int64_t*)s;
    const char *data = s + sizeof(int64_t);
    fwrite(data, 1, len, stdout);
}

void spy_print_newline(void) {
    printf("\n");
}

// ==================== Strings ====================
// Layout: [int64_t len][char data...]

// General GC allocation, used for closure environments (and any other
// codegen-synthesized heap blocks that hold pointers).
void* spy_gc_alloc(int64_t n) {
    return GC_MALLOC((size_t)(n > 0 ? n : 1));
}

char* spy_str_new(const char *data, int64_t len) {
    char *s = GC_MALLOC_ATOMIC(sizeof(int64_t) + len);
    *(int64_t*)s = len;
    memcpy(s + sizeof(int64_t), data, len);
    return s;
}

char* spy_str_concat(const char *a, const char *b) {
    int64_t len_a = *(int64_t*)a;
    int64_t len_b = *(int64_t*)b;
    int64_t total = len_a + len_b;
    char *result = GC_MALLOC_ATOMIC(sizeof(int64_t) + total);
    *(int64_t*)result = total;
    memcpy(result + sizeof(int64_t), a + sizeof(int64_t), len_a);
    memcpy(result + sizeof(int64_t) + len_a, b + sizeof(int64_t), len_b);
    return result;
}

int spy_str_eq(const char *a, const char *b) {
    int64_t len_a = *(int64_t*)a;
    int64_t len_b = *(int64_t*)b;
    if (len_a != len_b) return 0;
    return memcmp(a + sizeof(int64_t), b + sizeof(int64_t), len_a) == 0;
}

char* spy_str_index(const char *s, int64_t i) {
    int64_t len = *(int64_t*)s;
    if (i < 0 || i >= len) {
        fprintf(stderr, "index out of range: %lld (length %lld)\n", (long long)i, (long long)len);
        exit(1);
    }
    char ch = (s + sizeof(int64_t))[i];
    return spy_str_new(&ch, 1);
}

int64_t spy_str_len(const char *s) {
    return *(int64_t*)s;
}

int64_t spy_str_compare(const char *a, const char *b) {
    int64_t len_a = *(int64_t*)a;
    int64_t len_b = *(int64_t*)b;
    int64_t min_len = len_a < len_b ? len_a : len_b;
    int cmp = memcmp(a + sizeof(int64_t), b + sizeof(int64_t), min_len);
    if (cmp != 0) return cmp;
    if (len_a < len_b) return -1;
    if (len_a > len_b) return 1;
    return 0;
}

// ==================== String methods ====================
// ASCII semantics (spython strings are byte strings). These back the
// str.<method> surface exposed by the type checker / codegen.

#define SPY_SDATA(s) ((const char*)((s) + sizeof(int64_t)))

static int spy_is_space_ch(unsigned char c) {
    return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f';
}

char* spy_str_upper(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    char *out = GC_MALLOC_ATOMIC(sizeof(int64_t) + n);
    *(int64_t*)out = n;
    char *o = out + sizeof(int64_t);
    for (int64_t i = 0; i < n; i++) {
        unsigned char c = (unsigned char)d[i];
        o[i] = (c >= 'a' && c <= 'z') ? (char)(c - 32) : (char)c;
    }
    return out;
}

char* spy_str_lower(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    char *out = GC_MALLOC_ATOMIC(sizeof(int64_t) + n);
    *(int64_t*)out = n;
    char *o = out + sizeof(int64_t);
    for (int64_t i = 0; i < n; i++) {
        unsigned char c = (unsigned char)d[i];
        o[i] = (c >= 'A' && c <= 'Z') ? (char)(c + 32) : (char)c;
    }
    return out;
}

// capitalize: first char upper, rest lower.
char* spy_str_capitalize(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    char *out = GC_MALLOC_ATOMIC(sizeof(int64_t) + n);
    *(int64_t*)out = n;
    char *o = out + sizeof(int64_t);
    for (int64_t i = 0; i < n; i++) {
        unsigned char c = (unsigned char)d[i];
        if (i == 0) o[i] = (c >= 'a' && c <= 'z') ? (char)(c - 32) : (char)c;
        else o[i] = (c >= 'A' && c <= 'Z') ? (char)(c + 32) : (char)c;
    }
    return out;
}

char* spy_str_strip(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    int64_t lo = 0, hi = n;
    while (lo < hi && spy_is_space_ch((unsigned char)d[lo])) lo++;
    while (hi > lo && spy_is_space_ch((unsigned char)d[hi - 1])) hi--;
    return spy_str_new(d + lo, hi - lo);
}

char* spy_str_lstrip(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    int64_t lo = 0;
    while (lo < n && spy_is_space_ch((unsigned char)d[lo])) lo++;
    return spy_str_new(d + lo, n - lo);
}

char* spy_str_rstrip(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    int64_t hi = n;
    while (hi > 0 && spy_is_space_ch((unsigned char)d[hi - 1])) hi--;
    return spy_str_new(d, hi);
}

int spy_str_startswith(const char *s, const char *prefix) {
    int64_t n = *(int64_t*)s;
    int64_t pn = *(int64_t*)prefix;
    if (pn > n) return 0;
    return memcmp(SPY_SDATA(s), SPY_SDATA(prefix), pn) == 0;
}

int spy_str_endswith(const char *s, const char *suffix) {
    int64_t n = *(int64_t*)s;
    int64_t sn = *(int64_t*)suffix;
    if (sn > n) return 0;
    return memcmp(SPY_SDATA(s) + (n - sn), SPY_SDATA(suffix), sn) == 0;
}

// Lowest index of sub in s, or -1. Empty sub matches at 0.
int64_t spy_str_find(const char *s, const char *sub) {
    int64_t n = *(int64_t*)s;
    int64_t m = *(int64_t*)sub;
    const char *sd = SPY_SDATA(s);
    const char *td = SPY_SDATA(sub);
    if (m == 0) return 0;
    for (int64_t i = 0; i + m <= n; i++) {
        if (memcmp(sd + i, td, m) == 0) return i;
    }
    return -1;
}

int64_t spy_str_rfind(const char *s, const char *sub) {
    int64_t n = *(int64_t*)s;
    int64_t m = *(int64_t*)sub;
    const char *sd = SPY_SDATA(s);
    const char *td = SPY_SDATA(sub);
    if (m == 0) return n;
    for (int64_t i = n - m; i >= 0; i--) {
        if (memcmp(sd + i, td, m) == 0) return i;
    }
    return -1;
}

// Count of non-overlapping occurrences of sub in s.
int64_t spy_str_count(const char *s, const char *sub) {
    int64_t n = *(int64_t*)s;
    int64_t m = *(int64_t*)sub;
    const char *sd = SPY_SDATA(s);
    const char *td = SPY_SDATA(sub);
    if (m == 0) return n + 1;
    int64_t cnt = 0;
    for (int64_t i = 0; i + m <= n; ) {
        if (memcmp(sd + i, td, m) == 0) { cnt++; i += m; }
        else i++;
    }
    return cnt;
}

char* spy_str_replace(const char *s, const char *oldp, const char *newp) {
    int64_t n = *(int64_t*)s;
    int64_t om = *(int64_t*)oldp;
    int64_t nm = *(int64_t*)newp;
    const char *sd = SPY_SDATA(s);
    const char *od = SPY_SDATA(oldp);
    const char *nd = SPY_SDATA(newp);
    if (om == 0) return spy_str_new(sd, n); // mirror: no replacement on empty old
    int64_t cnt = spy_str_count(s, oldp);
    int64_t out_len = n + cnt * (nm - om);
    char *out = GC_MALLOC_ATOMIC(sizeof(int64_t) + (out_len > 0 ? out_len : 1));
    *(int64_t*)out = out_len;
    char *o = out + sizeof(int64_t);
    int64_t j = 0;
    for (int64_t i = 0; i < n; ) {
        if (i + om <= n && memcmp(sd + i, od, om) == 0) {
            memcpy(o + j, nd, nm); j += nm; i += om;
        } else {
            o[j++] = sd[i++];
        }
    }
    return out;
}

char* spy_str_zfill(const char *s, int64_t width) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    if (width <= n) return spy_str_new(d, n);
    int64_t pad = width - n;
    char *out = GC_MALLOC_ATOMIC(sizeof(int64_t) + width);
    *(int64_t*)out = width;
    char *o = out + sizeof(int64_t);
    int64_t start = 0;
    // Keep a leading sign in front of the zero padding.
    if (n > 0 && (d[0] == '+' || d[0] == '-')) {
        o[0] = d[0];
        start = 1;
    }
    for (int64_t i = 0; i < pad; i++) o[start + i] = '0';
    memcpy(o + start + pad, d + start, n - start);
    return out;
}

// split: returns a list[str]. sep == "" means split on runs of whitespace
// (Python's str.split() with no argument).
char* spy_str_split(const char *s, const char *sep) {
    int64_t n = *(int64_t*)s;
    int64_t sn = *(int64_t*)sep;
    const char *d = SPY_SDATA(s);
    const char *sd = SPY_SDATA(sep);
    char *list = spy_list_new(sizeof(char*));
    if (sn == 0) {
        int64_t i = 0;
        while (i < n) {
            while (i < n && spy_is_space_ch((unsigned char)d[i])) i++;
            if (i >= n) break;
            int64_t start = i;
            while (i < n && !spy_is_space_ch((unsigned char)d[i])) i++;
            char *piece = spy_str_new(d + start, i - start);
            spy_list_append(list, (const char*)&piece);
        }
        return list;
    }
    int64_t start = 0;
    int64_t i = 0;
    while (i + sn <= n) {
        if (memcmp(d + i, sd, sn) == 0) {
            char *piece = spy_str_new(d + start, i - start);
            spy_list_append(list, (const char*)&piece);
            i += sn;
            start = i;
        } else {
            i++;
        }
    }
    char *last = spy_str_new(d + start, n - start);
    spy_list_append(list, (const char*)&last);
    return list;
}

// join: sep.join(parts). parts is a list[str].
char* spy_str_join(const char *sep, const char *list_ptr) {
    int64_t cnt = spy_list_len(list_ptr);
    int64_t sn = *(int64_t*)sep;
    const char *sd = SPY_SDATA(sep);
    int64_t total = 0;
    for (int64_t i = 0; i < cnt; i++) {
        char *p = *(char**)spy_list_get(list_ptr, i);
        total += *(int64_t*)p;
        if (i > 0) total += sn;
    }
    char *out = GC_MALLOC_ATOMIC(sizeof(int64_t) + (total > 0 ? total : 1));
    *(int64_t*)out = total;
    char *o = out + sizeof(int64_t);
    int64_t j = 0;
    for (int64_t i = 0; i < cnt; i++) {
        if (i > 0) { memcpy(o + j, sd, sn); j += sn; }
        char *p = *(char**)spy_list_get(list_ptr, i);
        int64_t pn = *(int64_t*)p;
        memcpy(o + j, SPY_SDATA(p), pn);
        j += pn;
    }
    return out;
}

int spy_str_isdigit(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    if (n == 0) return 0;
    for (int64_t i = 0; i < n; i++)
        if (d[i] < '0' || d[i] > '9') return 0;
    return 1;
}

int spy_str_isalpha(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    if (n == 0) return 0;
    for (int64_t i = 0; i < n; i++) {
        unsigned char c = (unsigned char)d[i];
        if (!((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'))) return 0;
    }
    return 1;
}

int spy_str_isspace(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    if (n == 0) return 0;
    for (int64_t i = 0; i < n; i++)
        if (!spy_is_space_ch((unsigned char)d[i])) return 0;
    return 1;
}

// isupper/islower: true iff there is at least one cased char and none of
// the opposite case (matching CPython).
int spy_str_isupper(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    int cased = 0;
    for (int64_t i = 0; i < n; i++) {
        unsigned char c = (unsigned char)d[i];
        if (c >= 'a' && c <= 'z') return 0;
        if (c >= 'A' && c <= 'Z') cased = 1;
    }
    return cased;
}

int spy_str_islower(const char *s) {
    int64_t n = *(int64_t*)s;
    const char *d = SPY_SDATA(s);
    int cased = 0;
    for (int64_t i = 0; i < n; i++) {
        unsigned char c = (unsigned char)d[i];
        if (c >= 'A' && c <= 'Z') return 0;
        if (c >= 'a' && c <= 'z') cased = 1;
    }
    return cased;
}

// ==================== Slicing ====================
// Python-style slice index normalization. Resolves missing low/high/step (per
// `flags`), folds negative indices, clamps out-of-range values, and reports
// the number of elements the resulting slice will yield. After this returns,
// `*low_out` is the first index to copy and `*high_out` is the (exclusive on
// the iteration side) bound; the caller iterates `i = low_out; i +=/-= step;`
// for `out_len` steps.
//
// `length` is the length of the source sequence.
static int64_t spy_slice_resolve(int64_t length, int64_t flags,
                                 int64_t low, int64_t high, int64_t step,
                                 int64_t *low_out, int64_t *high_out,
                                 int64_t *step_out) {
    if (!(flags & 4)) step = 1;
    if (step == 0) {
        fprintf(stderr, "slice step cannot be zero\n");
        exit(1);
    }
    int64_t lo;
    if (flags & 1) {
        lo = low;
        if (lo < 0) lo += length;
        if (step > 0) {
            if (lo < 0) lo = 0;
            if (lo > length) lo = length;
        } else {
            if (lo < 0) lo = -1;
            if (lo >= length) lo = length - 1;
        }
    } else {
        lo = (step > 0) ? 0 : length - 1;
    }
    int64_t hi;
    if (flags & 2) {
        hi = high;
        if (hi < 0) hi += length;
        if (step > 0) {
            if (hi < 0) hi = 0;
            if (hi > length) hi = length;
        } else {
            if (hi < 0) hi = -1;
            if (hi >= length) hi = length - 1;
        }
    } else {
        hi = (step > 0) ? length : -1;
    }
    int64_t out_len;
    if (step > 0) {
        out_len = (hi > lo) ? (hi - lo + step - 1) / step : 0;
    } else {
        int64_t pos = -step;
        out_len = (lo > hi) ? (lo - hi + pos - 1) / pos : 0;
    }
    *low_out = lo;
    *high_out = hi;
    *step_out = step;
    return out_len;
}

char* spy_str_slice(const char *s, int64_t low, int64_t high, int64_t step, int64_t flags) {
    int64_t length = *(int64_t*)s;
    int64_t lo, hi, st;
    int64_t out_len = spy_slice_resolve(length, flags, low, high, step, &lo, &hi, &st);
    if (out_len <= 0) {
        return spy_str_new("", 0);
    }
    const char *data = s + sizeof(int64_t);
    if (st == 1) {
        // Fast path: contiguous copy.
        return spy_str_new(data + lo, out_len);
    }
    char *buf = (char*)malloc((size_t)out_len);
    int64_t i = lo;
    for (int64_t j = 0; j < out_len; j++) {
        buf[j] = data[i];
        i += st;
    }
    char *result = spy_str_new(buf, out_len);
    free(buf);
    return result;
}

// bytes share str's [int64_t len][data...] layout, so they slice identically.
char* spy_bytes_slice(const char *s, int64_t low, int64_t high, int64_t step, int64_t flags) {
    return spy_str_slice(s, low, high, step, flags);
}

// ==================== Lists ====================
// Layout: [int64_t len][int64_t cap][int64_t elem_size][char data...]

typedef struct {
    int64_t len;
    int64_t cap;
    int64_t elem_size;
    char *data;
} SpyList;

char* spy_list_new(int64_t elem_size) {
    SpyList *list = GC_MALLOC(sizeof(SpyList));
    list->len = 0;
    list->cap = 8;
    list->elem_size = elem_size;
    list->data = GC_MALLOC(list->cap * elem_size);
    return (char*)list;
}

void spy_list_append(char *list_ptr, const char *elem) {
    SpyList *list = (SpyList*)list_ptr;
    if (list->len >= list->cap) {
        list->cap *= 2;
        list->data = GC_REALLOC(list->data, list->cap * list->elem_size);
    }
    memcpy(list->data + list->len * list->elem_size, elem, list->elem_size);
    list->len++;
}

char* spy_list_get(const char *list_ptr, int64_t index) {
    SpyList *list = (SpyList*)list_ptr;
    if (index < 0 || index >= list->len) {
        fprintf(stderr, "list index out of range: %lld (length %lld)\n", (long long)index, (long long)list->len);
        exit(1);
    }
    return list->data + index * list->elem_size;
}

void spy_list_set(char *list_ptr, int64_t index, const char *elem) {
    SpyList *list = (SpyList*)list_ptr;
    if (index < 0 || index >= list->len) {
        fprintf(stderr, "list index out of range: %lld (length %lld)\n", (long long)index, (long long)list->len);
        exit(1);
    }
    memcpy(list->data + index * list->elem_size, elem, list->elem_size);
}

int64_t spy_list_len(const char *list_ptr) {
    SpyList *list = (SpyList*)list_ptr;
    return list->len;
}

char* spy_list_slice(const char *list_ptr, int64_t low, int64_t high, int64_t step, int64_t flags) {
    SpyList *src = (SpyList*)list_ptr;
    int64_t lo, hi, st;
    int64_t out_len = spy_slice_resolve(src->len, flags, low, high, step, &lo, &hi, &st);
    char *new_list = spy_list_new(src->elem_size);
    SpyList *dst = (SpyList*)new_list;
    if (out_len <= 0) return new_list;
    if (out_len > dst->cap) {
        dst->cap = out_len;
        dst->data = GC_REALLOC(dst->data, dst->cap * dst->elem_size);
    }
    if (st == 1) {
        memcpy(dst->data, src->data + lo * src->elem_size, out_len * src->elem_size);
    } else {
        for (int64_t j = 0; j < out_len; j++) {
            int64_t i = lo + j * st;
            memcpy(dst->data + j * dst->elem_size,
                   src->data + i * src->elem_size,
                   dst->elem_size);
        }
    }
    dst->len = out_len;
    return new_list;
}

char* spy_bytearray_slice(const char *ba_ptr, int64_t low, int64_t high, int64_t step, int64_t flags) {
    return spy_list_slice(ba_ptr, low, high, step, flags);
}

// ---- list methods ----
// Element comparison: kind 1 means elements are spy_str pointers (compare by
// value); anything else compares the raw elem_size bytes (ints / floats /
// bools / pointer identity).
static int spy_list_elem_eq(const char *a, const char *b, int64_t elem_size, int64_t kind) {
    if (kind == 1) {
        return spy_str_eq(*(char* const*)a, *(char* const*)b);
    }
    return memcmp(a, b, elem_size) == 0;
}

// pop(): remove and return a pointer to the last element. The buffer keeps
// the bytes (only len shrinks), so the caller may read them immediately.
char* spy_list_pop(char *list_ptr) {
    SpyList *l = (SpyList*)list_ptr;
    if (l->len == 0) { fprintf(stderr, "pop from empty list\n"); exit(1); }
    l->len--;
    return l->data + l->len * l->elem_size;
}

void spy_list_insert(char *list_ptr, int64_t index, const char *elem) {
    SpyList *l = (SpyList*)list_ptr;
    if (index < 0) index = 0;
    if (index > l->len) index = l->len;
    if (l->len >= l->cap) { l->cap *= 2; l->data = GC_REALLOC(l->data, l->cap * l->elem_size); }
    memmove(l->data + (index + 1) * l->elem_size,
            l->data + index * l->elem_size,
            (l->len - index) * l->elem_size);
    memcpy(l->data + index * l->elem_size, elem, l->elem_size);
    l->len++;
}

int64_t spy_list_index(const char *list_ptr, const char *elem, int64_t kind) {
    SpyList *l = (SpyList*)list_ptr;
    for (int64_t i = 0; i < l->len; i++)
        if (spy_list_elem_eq(l->data + i * l->elem_size, elem, l->elem_size, kind)) return i;
    return -1;
}

int64_t spy_list_count_elem(const char *list_ptr, const char *elem, int64_t kind) {
    SpyList *l = (SpyList*)list_ptr;
    int64_t c = 0;
    for (int64_t i = 0; i < l->len; i++)
        if (spy_list_elem_eq(l->data + i * l->elem_size, elem, l->elem_size, kind)) c++;
    return c;
}

void spy_list_remove(char *list_ptr, const char *elem, int64_t kind) {
    SpyList *l = (SpyList*)list_ptr;
    for (int64_t i = 0; i < l->len; i++) {
        if (spy_list_elem_eq(l->data + i * l->elem_size, elem, l->elem_size, kind)) {
            memmove(l->data + i * l->elem_size,
                    l->data + (i + 1) * l->elem_size,
                    (l->len - i - 1) * l->elem_size);
            l->len--;
            return;
        }
    }
    fprintf(stderr, "list.remove(x): x not in list\n");
    exit(1);
}

void spy_list_reverse(char *list_ptr) {
    SpyList *l = (SpyList*)list_ptr;
    int64_t es = l->elem_size;
    for (int64_t i = 0, j = l->len - 1; i < j; i++, j--) {
        for (int64_t k = 0; k < es; k++) {
            char t = l->data[i * es + k];
            l->data[i * es + k] = l->data[j * es + k];
            l->data[j * es + k] = t;
        }
    }
}

void spy_list_clear(char *list_ptr) {
    ((SpyList*)list_ptr)->len = 0;
}

void spy_list_extend(char *dst_ptr, const char *src_ptr) {
    SpyList *src = (SpyList*)src_ptr;
    for (int64_t i = 0; i < src->len; i++)
        spy_list_append(dst_ptr, src->data + i * src->elem_size);
}

static int spy_cmp_i64(const void *a, const void *b) {
    int64_t x = *(const int64_t*)a, y = *(const int64_t*)b;
    return (x > y) - (x < y);
}
static int spy_cmp_f64(const void *a, const void *b) {
    double x = *(const double*)a, y = *(const double*)b;
    return (x > y) - (x < y);
}
static int spy_cmp_str(const void *a, const void *b) {
    int64_t c = spy_str_compare(*(char* const*)a, *(char* const*)b);
    return c < 0 ? -1 : (c > 0 ? 1 : 0);
}

// sort(): kind 0 = int64 ascending, 1 = double ascending, 2 = str ascending.
void spy_list_sort(char *list_ptr, int64_t kind) {
    SpyList *l = (SpyList*)list_ptr;
    if (kind == 1) qsort(l->data, l->len, l->elem_size, spy_cmp_f64);
    else if (kind == 2) qsort(l->data, l->len, l->elem_size, spy_cmp_str);
    else qsort(l->data, l->len, l->elem_size, spy_cmp_i64);
}

// ---- sort(key=..., reverse=...) ----
// The comparator sorts an index permutation by comparing precomputed keys.
// State is global because the C standard qsort comparator carries no context;
// safe here because key computation (which is what calls user closures)
// completes before qsort runs, and the comparator itself calls no closures.
static int64_t *g_sk_ki;   // int64 keys
static double  *g_sk_kf;   // double keys
static char   **g_sk_ks;   // str-pointer keys (length-prefixed)
static int64_t  g_sk_kind; // 0 int, 1 float, 2 str
static int      g_sk_reverse;
static int spy_sort_idx_cmp(const void *pa, const void *pb) {
    int64_t ia = *(const int64_t*)pa, ib = *(const int64_t*)pb;
    int r = 0;
    if (g_sk_kind == 1) {
        double a = g_sk_kf[ia], b = g_sk_kf[ib];
        r = (a > b) - (a < b);
    } else if (g_sk_kind == 2) {
        int64_t c = spy_str_compare(g_sk_ks[ia], g_sk_ks[ib]);
        r = c < 0 ? -1 : (c > 0 ? 1 : 0);
    } else {
        int64_t a = g_sk_ki[ia], b = g_sk_ki[ib];
        r = (a > b) - (a < b);
    }
    if (g_sk_reverse) r = -r;
    // Stable: equal keys keep original (ascending-index) order regardless of
    // reverse, matching CPython's stable sort.
    if (r == 0) return (ia > ib) - (ia < ib);
    return r;
}

// spy_list_sort_key: sort `list` in place using a key. When `closure` is
// non-null it is a spython callable value ([fnptr][captures...]); the key for
// each element is closure(element). When null, the element itself is the key.
// elem_kind/key_kind: 0 = int64, 1 = double, 2 = str pointer. reverse != 0
// sorts descending.
void spy_list_sort_key(char *list_ptr, char *closure, int64_t elem_kind,
                       int64_t key_kind, int64_t reverse) {
    SpyList *l = (SpyList*)list_ptr;
    int64_t n = l->len;
    if (n < 2) return;
    int64_t es = l->elem_size;

    int64_t *ki = NULL; double *kf = NULL; char **ks = NULL;
    if (key_kind == 1) kf = (double*)malloc((size_t)n * sizeof(double));
    else if (key_kind == 2) ks = (char**)malloc((size_t)n * sizeof(char*));
    else ki = (int64_t*)malloc((size_t)n * sizeof(int64_t));

    void *fnp = closure ? *(void**)closure : NULL;

    for (int64_t i = 0; i < n; i++) {
        char *slot = l->data + i * es;
        if (fnp == NULL) {
            if (key_kind == 1) kf[i] = *(double*)slot;
            else if (key_kind == 2) ks[i] = *(char**)slot;
            else ki[i] = *(int64_t*)slot;
            continue;
        }
        if (elem_kind == 1) {
            double e = *(double*)slot;
            if (key_kind == 0) ki[i] = ((int64_t(*)(char*,double))fnp)(closure, e);
            else if (key_kind == 1) kf[i] = ((double(*)(char*,double))fnp)(closure, e);
            else ks[i] = ((char*(*)(char*,double))fnp)(closure, e);
        } else if (elem_kind == 2) {
            char *e = *(char**)slot;
            if (key_kind == 0) ki[i] = ((int64_t(*)(char*,char*))fnp)(closure, e);
            else if (key_kind == 1) kf[i] = ((double(*)(char*,char*))fnp)(closure, e);
            else ks[i] = ((char*(*)(char*,char*))fnp)(closure, e);
        } else {
            int64_t e = *(int64_t*)slot;
            if (key_kind == 0) ki[i] = ((int64_t(*)(char*,int64_t))fnp)(closure, e);
            else if (key_kind == 1) kf[i] = ((double(*)(char*,int64_t))fnp)(closure, e);
            else ks[i] = ((char*(*)(char*,int64_t))fnp)(closure, e);
        }
    }

    int64_t *idx = (int64_t*)malloc((size_t)n * sizeof(int64_t));
    for (int64_t i = 0; i < n; i++) idx[i] = i;
    g_sk_ki = ki; g_sk_kf = kf; g_sk_ks = ks;
    g_sk_kind = key_kind; g_sk_reverse = reverse != 0;
    qsort(idx, (size_t)n, sizeof(int64_t), spy_sort_idx_cmp);

    char *tmp = (char*)malloc((size_t)n * es);
    for (int64_t i = 0; i < n; i++)
        memcpy(tmp + i * es, l->data + idx[i] * es, es);
    memcpy(l->data, tmp, (size_t)n * es);

    free(tmp);
    free(idx);
    free(ki);
    free(kf);
    free(ks);
}

// ==================== Maps ====================
// Simple open-addressing hash map

#define MAP_INIT_CAP 16
#define MAP_LOAD_FACTOR 0.75

typedef struct {
    char *key;
    char *value;
    int occupied;
} MapEntry;

typedef struct {
    int64_t key_size;
    int64_t val_size;
    int64_t hash_type; // 0=int, 1=str
    int64_t len;
    int64_t cap;
    MapEntry *entries;
} SpyMap;

static uint64_t hash_int(const char *key, int64_t key_size) {
    uint64_t val = 0;
    memcpy(&val, key, key_size < 8 ? key_size : 8);
    val = (val ^ (val >> 30)) * 0xbf58476d1ce4e5b9ULL;
    val = (val ^ (val >> 27)) * 0x94d049bb133111ebULL;
    return val ^ (val >> 31);
}

static uint64_t hash_str(const char *key, int64_t key_size) {
    (void)key_size;
    // key is a pointer to i8* (spy_str pointer)
    char *str_ptr = *(char**)key;
    int64_t len = *(int64_t*)str_ptr;
    const char *data = str_ptr + sizeof(int64_t);
    uint64_t hash = 5381;
    for (int64_t i = 0; i < len; i++) {
        hash = ((hash << 5) + hash) + (unsigned char)data[i];
    }
    return hash;
}

static int key_eq_int(const char *a, const char *b, int64_t key_size) {
    return memcmp(a, b, key_size) == 0;
}

static int key_eq_str(const char *a, const char *b, int64_t key_size) {
    (void)key_size;
    char *str_a = *(char**)a;
    char *str_b = *(char**)b;
    return spy_str_eq(str_a, str_b);
}

char* spy_map_new(int64_t key_size, int64_t val_size, int64_t hash_type) {
    SpyMap *map = GC_MALLOC(sizeof(SpyMap));
    map->key_size = key_size;
    map->val_size = val_size;
    map->hash_type = hash_type;
    map->len = 0;
    map->cap = MAP_INIT_CAP;
    map->entries = GC_MALLOC(MAP_INIT_CAP * sizeof(MapEntry));
    return (char*)map;
}

static void map_resize(SpyMap *map);

static uint64_t map_hash(SpyMap *map, const char *key) {
    if (map->hash_type == 1) {
        return hash_str(key, map->key_size);
    }
    return hash_int(key, map->key_size);
}

static int map_key_eq(SpyMap *map, const char *a, const char *b) {
    if (map->hash_type == 1) {
        return key_eq_str(a, b, map->key_size);
    }
    return key_eq_int(a, b, map->key_size);
}

void spy_map_set(char *map_ptr, const char *key, const char *val) {
    SpyMap *map = (SpyMap*)map_ptr;

    if ((double)(map->len + 1) / (double)map->cap > MAP_LOAD_FACTOR) {
        map_resize(map);
    }

    uint64_t h = map_hash(map, key) % map->cap;
    for (int64_t i = 0; i < map->cap; i++) {
        int64_t idx = (h + i) % map->cap;
        MapEntry *entry = &map->entries[idx];
        if (!entry->occupied) {
            entry->key = GC_MALLOC(map->key_size);
            memcpy(entry->key, key, map->key_size);
            entry->value = GC_MALLOC(map->val_size);
            memcpy(entry->value, val, map->val_size);
            entry->occupied = 1;
            map->len++;
            return;
        }
        if (map_key_eq(map, entry->key, key)) {
            memcpy(entry->value, val, map->val_size);
            return;
        }
    }
}

char* spy_map_get(const char *map_ptr, const char *key) {
    SpyMap *map = (SpyMap*)map_ptr;
    uint64_t h = map_hash(map, key) % map->cap;
    for (int64_t i = 0; i < map->cap; i++) {
        int64_t idx = (h + i) % map->cap;
        MapEntry *entry = &map->entries[idx];
        if (!entry->occupied) {
            fprintf(stderr, "key not found in map\n");
            exit(1);
        }
        if (map_key_eq(map, entry->key, key)) {
            return entry->value;
        }
    }
    fprintf(stderr, "key not found in map\n");
    exit(1);
}

int spy_map_contains(const char *map_ptr, const char *key) {
    SpyMap *map = (SpyMap*)map_ptr;
    uint64_t h = map_hash(map, key) % map->cap;
    for (int64_t i = 0; i < map->cap; i++) {
        int64_t idx = (h + i) % map->cap;
        MapEntry *entry = &map->entries[idx];
        if (!entry->occupied) return 0;
        if (map_key_eq(map, entry->key, key)) return 1;
    }
    return 0;
}

int64_t spy_map_len(const char *map_ptr) {
    SpyMap *map = (SpyMap*)map_ptr;
    return map->len;
}

// Iteration helpers, mirroring spy_set_next/spy_set_key. Walk by passing
// the returned index back in, starting with -1; -1 also signals "done".
// spy_map_key_at / spy_map_val_at return the same pointer the entry holds
// — for hash_type=1 (str) the key slot stores a char* (pointer-to-pointer
// to spy_str), the layer above must dereference accordingly. Useful for
// json.dumps and any future generic map walker.
int64_t spy_map_next(const char *map_ptr, int64_t prev) {
    SpyMap *map = (SpyMap*)map_ptr;
    for (int64_t i = prev + 1; i < map->cap; i++) {
        if (map->entries[i].occupied) return i;
    }
    return -1;
}

char* spy_map_key_at(const char *map_ptr, int64_t idx) {
    SpyMap *map = (SpyMap*)map_ptr;
    return map->entries[idx].key;
}

char* spy_map_val_at(const char *map_ptr, int64_t idx) {
    SpyMap *map = (SpyMap*)map_ptr;
    return map->entries[idx].value;
}

void spy_map_extend(char *dst_ptr, const char *src_ptr) {
    SpyMap *src = (SpyMap*)src_ptr;
    for (int64_t i = 0; i < src->cap; i++) {
        if (src->entries[i].occupied) {
            spy_map_set(dst_ptr, src->entries[i].key, src->entries[i].value);
        }
    }
}

// keys()/values(): materialize a list[K] / list[V]. Iteration order is the
// internal table order (CPython preserves insertion order; spython does not).
char* spy_map_keys(const char *map_ptr) {
    SpyMap *map = (SpyMap*)map_ptr;
    char *list = spy_list_new(map->key_size);
    for (int64_t i = 0; i < map->cap; i++)
        if (map->entries[i].occupied) spy_list_append(list, map->entries[i].key);
    return list;
}

char* spy_map_values(const char *map_ptr) {
    SpyMap *map = (SpyMap*)map_ptr;
    char *list = spy_list_new(map->val_size);
    for (int64_t i = 0; i < map->cap; i++)
        if (map->entries[i].occupied) spy_list_append(list, map->entries[i].value);
    return list;
}

// get(key, default): pointer to the stored value, or to the default if the
// key is absent.
char* spy_map_get_or(const char *map_ptr, const char *key, const char *defptr) {
    SpyMap *map = (SpyMap*)map_ptr;
    uint64_t h = map_hash(map, key) % map->cap;
    for (int64_t i = 0; i < map->cap; i++) {
        int64_t idx = (h + i) % map->cap;
        MapEntry *entry = &map->entries[idx];
        if (!entry->occupied) return (char*)defptr;
        if (map_key_eq(map, entry->key, key)) return entry->value;
    }
    return (char*)defptr;
}

void spy_map_clear(char *map_ptr) {
    SpyMap *map = (SpyMap*)map_ptr;
    for (int64_t i = 0; i < map->cap; i++) map->entries[i].occupied = 0;
    map->len = 0;
}

static void map_resize(SpyMap *map) {
    int64_t old_cap = map->cap;
    MapEntry *old_entries = map->entries;

    map->cap *= 2;
    map->entries = GC_MALLOC(map->cap * sizeof(MapEntry));
    map->len = 0;

    for (int64_t i = 0; i < old_cap; i++) {
        if (old_entries[i].occupied) {
            spy_map_set((char*)map, old_entries[i].key, old_entries[i].value);
        }
    }
}

// ==================== Sets ====================
// Open-addressing hash set; mirrors SpyMap with keys only.

typedef struct {
    char *key;
    int occupied;
} SetEntry;

typedef struct {
    int64_t key_size;
    int64_t hash_type; // 0=int, 1=str
    int64_t len;
    int64_t cap;
    SetEntry *entries;
} SpySet;

static void set_resize(SpySet *set);

static uint64_t set_hash(SpySet *set, const char *key) {
    if (set->hash_type == 1) {
        return hash_str(key, set->key_size);
    }
    return hash_int(key, set->key_size);
}

static int set_key_eq(SpySet *set, const char *a, const char *b) {
    if (set->hash_type == 1) {
        return key_eq_str(a, b, set->key_size);
    }
    return key_eq_int(a, b, set->key_size);
}

char* spy_set_new(int64_t key_size, int64_t hash_type) {
    SpySet *set = GC_MALLOC(sizeof(SpySet));
    set->key_size = key_size;
    set->hash_type = hash_type;
    set->len = 0;
    set->cap = MAP_INIT_CAP;
    set->entries = GC_MALLOC(MAP_INIT_CAP * sizeof(SetEntry));
    return (char*)set;
}

void spy_set_add(char *set_ptr, const char *key) {
    SpySet *set = (SpySet*)set_ptr;

    if ((double)(set->len + 1) / (double)set->cap > MAP_LOAD_FACTOR) {
        set_resize(set);
    }

    uint64_t h = set_hash(set, key) % set->cap;
    for (int64_t i = 0; i < set->cap; i++) {
        int64_t idx = (h + i) % set->cap;
        SetEntry *entry = &set->entries[idx];
        if (!entry->occupied) {
            entry->key = GC_MALLOC(set->key_size);
            memcpy(entry->key, key, set->key_size);
            entry->occupied = 1;
            set->len++;
            return;
        }
        if (set_key_eq(set, entry->key, key)) {
            return; // already present
        }
    }
}

int spy_set_contains(const char *set_ptr, const char *key) {
    SpySet *set = (SpySet*)set_ptr;
    uint64_t h = set_hash(set, key) % set->cap;
    for (int64_t i = 0; i < set->cap; i++) {
        int64_t idx = (h + i) % set->cap;
        SetEntry *entry = &set->entries[idx];
        if (!entry->occupied) return 0;
        if (set_key_eq(set, entry->key, key)) return 1;
    }
    return 0;
}

// Mark a slot empty by walking probe chain. Subsequent collisions for keys
// that hashed past this slot continue to find their entries because we do
// not move them — discard is a tombstone-free remove that works for our
// load-factor-bounded table.
void spy_set_discard(char *set_ptr, const char *key) {
    SpySet *set = (SpySet*)set_ptr;
    uint64_t h = set_hash(set, key) % set->cap;
    for (int64_t i = 0; i < set->cap; i++) {
        int64_t idx = (h + i) % set->cap;
        SetEntry *entry = &set->entries[idx];
        if (!entry->occupied) return;
        if (set_key_eq(set, entry->key, key)) {
            // Re-insert subsequent occupied entries in the probe chain so
            // future lookups still find them.
            entry->occupied = 0;
            entry->key = NULL;
            set->len--;
            for (int64_t j = i + 1; j < set->cap; j++) {
                int64_t jidx = (h + j) % set->cap;
                SetEntry *next = &set->entries[jidx];
                if (!next->occupied) return;
                char *rk = next->key;
                next->occupied = 0;
                next->key = NULL;
                set->len--;
                spy_set_add(set_ptr, rk);
            }
            return;
        }
    }
}

int64_t spy_set_len(const char *set_ptr) {
    SpySet *set = (SpySet*)set_ptr;
    return set->len;
}

// Iteration: return the bucket index of the next occupied slot at or after
// `start`, or -1 when none. The compiler emits `for x in s` as a loop that
// calls spy_set_next() and reads the key via spy_set_key().
int64_t spy_set_next(const char *set_ptr, int64_t start) {
    SpySet *set = (SpySet*)set_ptr;
    for (int64_t i = start; i < set->cap; i++) {
        if (set->entries[i].occupied) return i;
    }
    return -1;
}

char* spy_set_key(const char *set_ptr, int64_t idx) {
    SpySet *set = (SpySet*)set_ptr;
    return set->entries[idx].key;
}

static void set_resize(SpySet *set) {
    int64_t old_cap = set->cap;
    SetEntry *old_entries = set->entries;

    set->cap *= 2;
    set->entries = GC_MALLOC(set->cap * sizeof(SetEntry));
    set->len = 0;

    for (int64_t i = 0; i < old_cap; i++) {
        if (old_entries[i].occupied) {
            spy_set_add((char*)set, old_entries[i].key);
        }
    }
}

// ==================== Conversions ====================

char* spy_int_to_str(int64_t x) {
    char buf[32];
    int len = snprintf(buf, sizeof(buf), "%lld", (long long)x);
    return spy_str_new(buf, len);
}

char* spy_float_to_str(double x) {
    char buf[64];
    int len = spy_format_float(x, buf, sizeof(buf));
    return spy_str_new(buf, len);
}

char* spy_bool_to_str(int x) {
    if (x) return spy_str_new("True", 4);
    return spy_str_new("False", 5);
}

// ==================== Instances ====================

// Zero-initialized heap allocation for a class instance. The returned pointer
// is the raw memory; the compiler fills in the vtable pointer at offset 0 and
// initializes fields via __init__.
char* spy_instance_new(int64_t size) {
    void *p = GC_MALLOC((size_t)size);
    if (!p) {
        fprintf(stderr, "out of memory\n");
        exit(1);
    }
    return (char*)p;
}

// ==================== Bytearray ====================
// A bytearray is a SpyList specialized to elem_size=1. The language-level
// operations (get/set/append) treat each slot as an unsigned byte, returned
// as int (zero-extended) at the language level.

char* spy_bytearray_new(int64_t len) {
    char *ba = spy_list_new(1);
    SpyList *list = (SpyList*)ba;
    // Ensure capacity and zero the first `len` bytes so indexing is defined
    // immediately after construction.
    if (len > list->cap) {
        list->cap = len;
        list->data = GC_REALLOC(list->data, list->cap);
    }
    for (int64_t i = 0; i < len; i++) list->data[i] = 0;
    list->len = len;
    return ba;
}

// Initialize a bytearray from an existing length-prefixed bytes/str buffer.
char* spy_bytearray_from_bytes(const char *src) {
    int64_t src_len = *(int64_t*)src;
    const char *src_data = src + sizeof(int64_t);
    char *ba = spy_bytearray_new(src_len);
    SpyList *list = (SpyList*)ba;
    memcpy(list->data, src_data, (size_t)src_len);
    return ba;
}

int64_t spy_bytearray_get(const char *ba_ptr, int64_t idx) {
    SpyList *list = (SpyList*)ba_ptr;
    if (idx < 0 || idx >= list->len) {
        fprintf(stderr, "bytearray index out of range: %lld (length %lld)\n",
                (long long)idx, (long long)list->len);
        exit(1);
    }
    return (int64_t)(unsigned char)list->data[idx];
}

void spy_bytearray_set(char *ba_ptr, int64_t idx, int64_t val) {
    SpyList *list = (SpyList*)ba_ptr;
    if (idx < 0 || idx >= list->len) {
        fprintf(stderr, "bytearray index out of range: %lld (length %lld)\n",
                (long long)idx, (long long)list->len);
        exit(1);
    }
    list->data[idx] = (char)(val & 0xff);
}

void spy_bytearray_append(char *ba_ptr, int64_t val) {
    SpyList *list = (SpyList*)ba_ptr;
    if (list->len >= list->cap) {
        list->cap = list->cap ? list->cap * 2 : 8;
        list->data = GC_REALLOC(list->data, list->cap);
    }
    list->data[list->len++] = (char)(val & 0xff);
}

// Copy a bytearray into a fresh immutable bytes value (same runtime layout as
// str: [int64_t len][data...]).
char* spy_bytearray_to_bytes(const char *ba_ptr) {
    SpyList *list = (SpyList*)ba_ptr;
    return spy_str_new(list->data, list->len);
}

int64_t spy_bytearray_len(const char *ba_ptr) {
    SpyList *list = (SpyList*)ba_ptr;
    return list->len;
}

// Parse a spython str into an int64. Empty input or any non-digit
// (other than a leading +/-) raises ValueError-shaped abort. Mirrors
// CPython int(str) for the simple decimal case; binary/hex prefixes
// are not recognised. Used by codegen for `int(s)` where s: str.
int64_t spy_str_to_int(const char *spy_str) {
    int64_t n = spy_str_len(spy_str);
    const char *d = spy_str + sizeof(int64_t);
    int64_t i = 0;
    int sign = 1;
    if (i < n && (d[i] == '+' || d[i] == '-')) {
        if (d[i] == '-') sign = -1;
        i++;
    }
    if (i >= n) {
        fprintf(stderr, "ValueError: int() with empty string\n");
        exit(1);
    }
    int64_t v = 0;
    while (i < n) {
        char c = d[i];
        if (c < '0' || c > '9') {
            fprintf(stderr, "ValueError: invalid literal for int(): '%.*s'\n",
                    (int)n, d);
            exit(1);
        }
        v = v * 10 + (c - '0');
        i++;
    }
    return v * sign;
}

double spy_str_to_float(const char *spy_str) {
    int64_t n = spy_str_len(spy_str);
    const char *d = spy_str + sizeof(int64_t);
    char tmp[64];
    if (n >= (int64_t)sizeof(tmp)) {
        char *big = (char*)malloc((size_t)(n + 1));
        memcpy(big, d, (size_t)n);
        big[n] = 0;
        double v = strtod(big, NULL);
        free(big);
        return v;
    }
    memcpy(tmp, d, (size_t)n);
    tmp[n] = 0;
    return strtod(tmp, NULL);
}

// ==================== Math ====================

int64_t spy_int_pow(int64_t base, int64_t exp) {
    if (exp < 0) return 0; // Integer negative power = 0
    int64_t result = 1;
    while (exp > 0) {
        if (exp & 1) result *= base;
        base *= base;
        exp >>= 1;
    }
    return result;
}

// ==================== Any (tagged value box) ====================

typedef struct SpyAny {
    int32_t tag;
    int32_t pad;
    int64_t payload; // scalar payload, double-bits, or pointer-as-int
} SpyAny;

static SpyAny *spy_any_alloc(int has_pointer_payload) {
    // GC_MALLOC_ATOMIC for scalar payloads tells Boehm there are no inner
    // pointers; GC_MALLOC for str/list/dict/bytes payloads so the GC keeps
    // the referent alive.
    SpyAny *a = (SpyAny*)(has_pointer_payload ? GC_MALLOC(sizeof(SpyAny))
                                              : GC_MALLOC_ATOMIC(sizeof(SpyAny)));
    a->pad = 0;
    return a;
}

char* spy_any_none(void) {
    SpyAny *a = spy_any_alloc(0);
    a->tag = SPY_ANY_NONE;
    a->payload = 0;
    return (char*)a;
}

char* spy_any_box_int(int64_t v) {
    SpyAny *a = spy_any_alloc(0);
    a->tag = SPY_ANY_INT;
    a->payload = v;
    return (char*)a;
}

char* spy_any_box_float(double v) {
    SpyAny *a = spy_any_alloc(0);
    a->tag = SPY_ANY_FLOAT;
    int64_t bits;
    memcpy(&bits, &v, sizeof(bits));
    a->payload = bits;
    return (char*)a;
}

char* spy_any_box_bool(int v) {
    SpyAny *a = spy_any_alloc(0);
    a->tag = SPY_ANY_BOOL;
    a->payload = v ? 1 : 0;
    return (char*)a;
}

char* spy_any_box_str(const char *s) {
    SpyAny *a = spy_any_alloc(1);
    a->tag = SPY_ANY_STR;
    a->payload = (int64_t)(uintptr_t)s;
    return (char*)a;
}

char* spy_any_box_list(const char *l) {
    SpyAny *a = spy_any_alloc(1);
    a->tag = SPY_ANY_LIST;
    a->payload = (int64_t)(uintptr_t)l;
    return (char*)a;
}

char* spy_any_box_map(const char *m) {
    SpyAny *a = spy_any_alloc(1);
    a->tag = SPY_ANY_DICT;
    a->payload = (int64_t)(uintptr_t)m;
    return (char*)a;
}

char* spy_any_box_bytes(const char *b) {
    SpyAny *a = spy_any_alloc(1);
    a->tag = SPY_ANY_BYTES;
    a->payload = (int64_t)(uintptr_t)b;
    return (char*)a;
}

int spy_any_tag(const char *a) {
    return ((const SpyAny*)a)->tag;
}

int spy_any_is_none(const char *a) {
    return ((const SpyAny*)a)->tag == SPY_ANY_NONE;
}

static const char *spy_any_tag_name(int tag) {
    switch (tag) {
        case SPY_ANY_NONE:  return "None";
        case SPY_ANY_INT:   return "int";
        case SPY_ANY_FLOAT: return "float";
        case SPY_ANY_BOOL:  return "bool";
        case SPY_ANY_STR:   return "str";
        case SPY_ANY_LIST:  return "list";
        case SPY_ANY_DICT:  return "dict";
        case SPY_ANY_BYTES: return "bytes";
        default:            return "<unknown>";
    }
}

static void spy_any_type_error(const char *want, int got) {
    fprintf(stderr, "TypeError: expected %s, got %s in Any unbox\n",
            want, spy_any_tag_name(got));
    exit(1);
}

int64_t spy_any_unbox_int(const char *a) {
    const SpyAny *s = (const SpyAny*)a;
    if (s->tag != SPY_ANY_INT) spy_any_type_error("int", s->tag);
    return s->payload;
}

double spy_any_unbox_float(const char *a) {
    const SpyAny *s = (const SpyAny*)a;
    if (s->tag != SPY_ANY_FLOAT) {
        // Allow int -> float (lossless) for ergonomic JSON parsing where
        // numbers are sometimes integers, sometimes floats.
        if (s->tag == SPY_ANY_INT) return (double)s->payload;
        spy_any_type_error("float", s->tag);
    }
    double v;
    int64_t bits = s->payload;
    memcpy(&v, &bits, sizeof(v));
    return v;
}

int spy_any_unbox_bool(const char *a) {
    const SpyAny *s = (const SpyAny*)a;
    if (s->tag != SPY_ANY_BOOL) spy_any_type_error("bool", s->tag);
    return (int)s->payload;
}

char* spy_any_unbox_str(const char *a) {
    const SpyAny *s = (const SpyAny*)a;
    if (s->tag != SPY_ANY_STR) spy_any_type_error("str", s->tag);
    return (char*)(uintptr_t)s->payload;
}

char* spy_any_unbox_list(const char *a) {
    const SpyAny *s = (const SpyAny*)a;
    if (s->tag != SPY_ANY_LIST) spy_any_type_error("list", s->tag);
    return (char*)(uintptr_t)s->payload;
}

char* spy_any_unbox_map(const char *a) {
    const SpyAny *s = (const SpyAny*)a;
    if (s->tag != SPY_ANY_DICT) spy_any_type_error("dict", s->tag);
    return (char*)(uintptr_t)s->payload;
}

char* spy_any_unbox_bytes(const char *a) {
    const SpyAny *s = (const SpyAny*)a;
    if (s->tag != SPY_ANY_BYTES) spy_any_type_error("bytes", s->tag);
    return (char*)(uintptr_t)s->payload;
}

// Cheap repr for debugging / generic str(any). Lists/dicts/bytes render as
// the bare type name with no contents — full pretty-printing requires a
// recursive walker that knows the element shapes.
char* spy_any_to_str(const char *a) {
    const SpyAny *s = (const SpyAny*)a;
    char buf[64];
    int len;
    switch (s->tag) {
        case SPY_ANY_NONE:
            return spy_str_new("None", 4);
        case SPY_ANY_INT:
            len = snprintf(buf, sizeof(buf), "%lld", (long long)s->payload);
            return spy_str_new(buf, len);
        case SPY_ANY_FLOAT: {
            double v;
            int64_t bits = s->payload;
            memcpy(&v, &bits, sizeof(v));
            len = spy_format_float(v, buf, sizeof(buf));
            return spy_str_new(buf, len);
        }
        case SPY_ANY_BOOL:
            return s->payload ? spy_str_new("True", 4) : spy_str_new("False", 5);
        case SPY_ANY_STR:
            return (char*)(uintptr_t)s->payload;
        case SPY_ANY_LIST:
            return spy_str_new("<list>", 6);
        case SPY_ANY_DICT:
            return spy_str_new("<dict>", 6);
        case SPY_ANY_BYTES:
            return spy_str_new("<bytes>", 7);
    }
    return spy_str_new("<any>", 5);
}
