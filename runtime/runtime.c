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

void spy_map_extend(char *dst_ptr, const char *src_ptr) {
    SpyMap *src = (SpyMap*)src_ptr;
    for (int64_t i = 0; i < src->cap; i++) {
        if (src->entries[i].occupied) {
            spy_map_set(dst_ptr, src->entries[i].key, src->entries[i].value);
        }
    }
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
