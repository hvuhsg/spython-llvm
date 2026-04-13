#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <gc.h>
#include "runtime.h"

// ==================== Initialization ====================

void spy_init(void) {
    GC_INIT();
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
