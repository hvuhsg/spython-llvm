#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <setjmp.h>
#include <gc.h>
#include "runtime.h"

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
    printf("%g", x);
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

// ==================== Conversions ====================

char* spy_int_to_str(int64_t x) {
    char buf[32];
    int len = snprintf(buf, sizeof(buf), "%lld", (long long)x);
    return spy_str_new(buf, len);
}

char* spy_float_to_str(double x) {
    char buf[64];
    int len = snprintf(buf, sizeof(buf), "%g", x);
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
