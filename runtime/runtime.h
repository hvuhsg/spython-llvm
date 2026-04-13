#ifndef SPYTHON_RUNTIME_H
#define SPYTHON_RUNTIME_H

#include <stdint.h>

// Initialization
void spy_init(void);

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

// Conversion functions
char* spy_int_to_str(int64_t x);
char* spy_float_to_str(double x);
char* spy_bool_to_str(int x);

// Instance allocation
char* spy_instance_new(int64_t size);

// Math
int64_t spy_int_pow(int64_t base, int64_t exp);

#endif
