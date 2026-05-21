// stdlib/requests.c — small C helpers for stdlib/requests.spy. The
// only reason this file exists is that spython has neither a `key in
// map` membership operator nor map iteration, both of which the
// HTTP-header dedup / emit logic in requests.spy needs.
//
// The header map is a SpyMap with hash_type=1 (str-keyed) and an 8-byte
// value slot also pointing at a spy_str. We walk it via the public
// spy_map_next / spy_map_key_at / spy_map_val_at iterators and emit
// `Name: Value\r\n` into a bytearray supplied by the caller.

#include <stdint.h>
#include <string.h>
#include "runtime.h"

#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

static void ba_append_str(char *ba, const char *spy_str) {
    int64_t n = spy_str_len(spy_str);
    const char *d = SPY_STR_DATA(spy_str);
    for (int64_t i = 0; i < n; i++) {
        spy_bytearray_append(ba, (int64_t)(unsigned char)d[i]);
    }
}

static void ba_append_lit(char *ba, const char *lit) {
    while (*lit) {
        spy_bytearray_append(ba, (int64_t)(unsigned char)*lit);
        lit++;
    }
}

void spy_requests__walk_headers(const char *headers, char *out) {
    int64_t idx = -1;
    while ((idx = spy_map_next(headers, idx)) >= 0) {
        char *key_slot = spy_map_key_at(headers, idx);
        char *val_slot = spy_map_val_at(headers, idx);
        // The slot stores `char*` (pointer to spy_str). Dereference to
        // recover the spy_str pointer that map literal codegen stored.
        const char *key = *(const char**)key_slot;
        const char *val = *(const char**)val_slot;
        ba_append_str(out, key);
        ba_append_lit(out, ": ");
        ba_append_str(out, val);
        ba_append_lit(out, "\r\n");
    }
}

int spy_requests__has_header(const char *headers, const char *key) {
    // Wrap the generic spy_map_contains, passing a pointer to the key
    // pointer (key_size=8, the slot stores char* not the spy_str
    // payload itself).
    return spy_map_contains(headers, (const char*)&key);
}
