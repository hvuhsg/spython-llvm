#ifndef SPYTHON_RUNTIME_H
#define SPYTHON_RUNTIME_H

// Public C ABI for spython.
//
// C-backed stdlib modules and user-authored C extensions include this header to
// interoperate with spython values. The ABI is:
//
//   int        <-> int64_t   (spython "int" = i64 in LLVM)
//   float      <-> double    (spython "float" = double)
//   bool       <-> int       (spython "bool" = i1; widened to int across the
//                             call boundary per the platform C ABI)
//   None       <-> void      (no return value)
//   str        <-> char*     (pointer to [int64_t len][data...]; use
//                             spy_str_new / spy_str_len / access payload at
//                             (s + sizeof(int64_t)))
//
// Naming convention: a C-backed module "foo" exports functions as
//   spy_foo_<name>
// spython's codegen emits calls to that mangled name by default. An @extern
// decorator with an explicit symbol argument overrides the default name.
//
// Memory: use GC_MALLOC / GC_MALLOC_ATOMIC from <gc.h> (Boehm GC). The runtime
// initializes GC on program start via spy_init().

#include <stdint.h>
#include <setjmp.h>

// Initialization
void spy_init(void);

// Process argv stash. Codegen wires spy_argv_set into main() so any C-backed
// module (or user code) can retrieve argv later via spy_argv_count/_at.
void spy_argv_set(int argc, char **argv);
int  spy_argv_count(void);
const char *spy_argv_at(int i);

// Exception handling.
//
// Frames are caller-allocated (LLVM alloca): the generated code reserves a
// 256-byte buffer per `try` and passes it as `frame`. The first
// sizeof(jmp_buf) bytes store the setjmp state; the rest are used by the
// runtime for the linked-list `prev` pointer. A 256-byte allocation is
// sufficient for any platform's jmp_buf + pointer (validated at build time
// by a _Static_assert in runtime.c).
//
// Contract:
//   - The caller emits: `spy_exc_push(frame)` then `setjmp(frame)`.
//     setjmp returning 0 means "normal entry", non-zero means "exception".
//   - On normal completion the caller emits `spy_exc_pop()`.
//   - On the exception path the caller emits `spy_exc_pop()` FIRST (so a
//     re-raise inside an `except` clause propagates to the parent handler),
//     then reads `spy_exc_current()` to get the raised object, dispatches,
//     and either calls `spy_exc_clear()` (handled) or `spy_exc_rethrow()`
//     (propagate).
//
// The in-flight exception pointer is registered as a Boehm GC root so the
// raised object stays alive across setjmp/longjmp.
typedef struct SpyExcFrame {
    jmp_buf buf;
    struct SpyExcFrame *prev;
} SpyExcFrame;

void  spy_exc_push(void *frame);
void  spy_exc_pop(void);
void *spy_exc_current(void);
void  spy_exc_clear(void);
void  spy_exc_throw(void *obj);   // noreturn; abort() if no handler installed
void  spy_exc_rethrow(void);      // noreturn

// Print functions
void spy_print_int(int64_t x);
void spy_print_float(double x);
void spy_print_bool(int x);
void spy_print_str(const char *s);
void spy_print_newline(void);

// String operations
void* spy_gc_alloc(int64_t n);
char* spy_str_new(const char *data, int64_t len);
char* spy_str_concat(const char *a, const char *b);
int spy_str_eq(const char *a, const char *b);
char* spy_str_index(const char *s, int64_t i);
int64_t spy_str_len(const char *s);
int64_t spy_str_compare(const char *a, const char *b);

// String methods (ASCII semantics). Back the str.<method> surface.
char* spy_str_upper(const char *s);
char* spy_str_lower(const char *s);
char* spy_str_capitalize(const char *s);
char* spy_str_strip(const char *s);
char* spy_str_lstrip(const char *s);
char* spy_str_rstrip(const char *s);
int spy_str_startswith(const char *s, const char *prefix);
int spy_str_endswith(const char *s, const char *suffix);
int64_t spy_str_find(const char *s, const char *sub);
int64_t spy_str_rfind(const char *s, const char *sub);
int64_t spy_str_count(const char *s, const char *sub);
char* spy_str_replace(const char *s, const char *oldp, const char *newp);
char* spy_str_zfill(const char *s, int64_t width);
char* spy_str_split(const char *s, const char *sep);
char* spy_str_join(const char *sep, const char *list_ptr);
int spy_str_isdigit(const char *s);
int spy_str_isalpha(const char *s);
int spy_str_isspace(const char *s);
int spy_str_isupper(const char *s);
int spy_str_islower(const char *s);

// Slice operations. `flags` encodes which of (low, high, step) were supplied
// at the call site: bit 0 = low present, bit 1 = high present, bit 2 = step
// present. Missing values use Python's defaults (which depend on step's sign).
// All four operate on the same length-prefixed-or-SpyList layouts already used
// elsewhere; the result is a freshly-allocated value of the same kind.
char* spy_str_slice(const char *s, int64_t low, int64_t high, int64_t step, int64_t flags);
char* spy_bytes_slice(const char *s, int64_t low, int64_t high, int64_t step, int64_t flags);
char* spy_list_slice(const char *list, int64_t low, int64_t high, int64_t step, int64_t flags);
char* spy_bytearray_slice(const char *ba, int64_t low, int64_t high, int64_t step, int64_t flags);

// List operations
char* spy_list_new(int64_t elem_size);
void spy_list_append(char *list, const char *elem);
char* spy_list_get(const char *list, int64_t index);
void spy_list_set(char *list, int64_t index, const char *elem);
int64_t spy_list_len(const char *list);

// list methods
char* spy_list_pop(char *list);
void spy_list_insert(char *list, int64_t index, const char *elem);
int64_t spy_list_index(const char *list, const char *elem, int64_t kind);
int64_t spy_list_count_elem(const char *list, const char *elem, int64_t kind);
void spy_list_remove(char *list, const char *elem, int64_t kind);
void spy_list_reverse(char *list);
void spy_list_clear(char *list);
void spy_list_extend(char *dst, const char *src);
void spy_list_sort(char *list, int64_t kind);
void spy_list_sort_key(char *list, char *closure, int64_t elem_kind,
                       int64_t key_kind, int64_t reverse);

// Map operations
char* spy_map_new(int64_t key_size, int64_t val_size, int64_t hash_type);
void spy_map_set(char *map, const char *key, const char *val);
char* spy_map_get(const char *map, const char *key);
int spy_map_contains(const char *map, const char *key);
int64_t spy_map_len(const char *map);
void spy_map_extend(char *dst, const char *src);
char* spy_map_keys(const char *map);
char* spy_map_values(const char *map);
char* spy_map_get_or(const char *map, const char *key, const char *defptr);
void spy_map_clear(char *map);
int64_t spy_map_next(const char *map, int64_t prev);
char* spy_map_key_at(const char *map, int64_t idx);
char* spy_map_val_at(const char *map, int64_t idx);

// Conversion functions
char* spy_int_to_str(int64_t x);
char* spy_float_to_str(double x);
char* spy_bool_to_str(int x);
int64_t spy_str_to_int(const char *s);
double spy_str_to_float(const char *s);

// Instance allocation
char* spy_instance_new(int64_t size);

// Bytearray operations (mutable byte buffer; each slot is an unsigned byte,
// exposed as int at the language level).
char* spy_bytearray_new(int64_t len);
char* spy_bytearray_from_bytes(const char *src);
int64_t spy_bytearray_get(const char *ba, int64_t idx);
void spy_bytearray_set(char *ba, int64_t idx, int64_t val);
void spy_bytearray_append(char *ba, int64_t val);
char* spy_bytearray_to_bytes(const char *ba);
int64_t spy_bytearray_len(const char *ba);

// Math
int64_t spy_int_pow(int64_t base, int64_t exp);

// Any (tagged value box).
//
// SpyAny is a 16-byte heap-allocated struct: a 4-byte tag, 4 bytes of
// padding, and an 8-byte payload. Scalar tags (int/float/bool) store the
// raw value bit-cast into payload; pointer tags (str/list/dict/bytes) store
// the value pointer. None has tag 0 with a zero payload.
//
// Box helpers allocate via GC_MALLOC_ATOMIC (no internal pointers for
// scalars) or GC_MALLOC (pointer payload, GC must trace). Unbox helpers
// raise TypeError when the runtime tag does not match the requested type.
#define SPY_ANY_NONE  0
#define SPY_ANY_INT   1
#define SPY_ANY_FLOAT 2
#define SPY_ANY_BOOL  3
#define SPY_ANY_STR   4
#define SPY_ANY_LIST  5
#define SPY_ANY_DICT  6
#define SPY_ANY_BYTES 7

char*   spy_any_none(void);
char*   spy_any_box_int(int64_t v);
char*   spy_any_box_float(double v);
char*   spy_any_box_bool(int v);
char*   spy_any_box_str(const char *s);
char*   spy_any_box_list(const char *l);
char*   spy_any_box_map(const char *m);
char*   spy_any_box_bytes(const char *b);
int     spy_any_tag(const char *a);
int     spy_any_is_none(const char *a);
int64_t spy_any_unbox_int(const char *a);
double  spy_any_unbox_float(const char *a);
int     spy_any_unbox_bool(const char *a);
char*   spy_any_unbox_str(const char *a);
char*   spy_any_unbox_list(const char *a);
char*   spy_any_unbox_map(const char *a);
char*   spy_any_unbox_bytes(const char *a);
char*   spy_any_to_str(const char *a);

#endif
