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
char* spy_str_new(const char *data, int64_t len);
char* spy_str_concat(const char *a, const char *b);
int spy_str_eq(const char *a, const char *b);
char* spy_str_index(const char *s, int64_t i);
int64_t spy_str_len(const char *s);
int64_t spy_str_compare(const char *a, const char *b);

// List operations
char* spy_list_new(int64_t elem_size);
void spy_list_append(char *list, const char *elem);
char* spy_list_get(const char *list, int64_t index);
void spy_list_set(char *list, int64_t index, const char *elem);
int64_t spy_list_len(const char *list);

// Map operations
char* spy_map_new(int64_t key_size, int64_t val_size, int64_t hash_type);
void spy_map_set(char *map, const char *key, const char *val);
char* spy_map_get(const char *map, const char *key);
int spy_map_contains(const char *map, const char *key);
int64_t spy_map_len(const char *map);
void spy_map_extend(char *dst, const char *src);

// Conversion functions
char* spy_int_to_str(int64_t x);
char* spy_float_to_str(double x);
char* spy_bool_to_str(int x);

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

#endif
