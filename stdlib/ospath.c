// stdlib/ospath.c — byte-level substring and rfind helpers for ospath.spy.
//
// Exists so path manipulation doesn't have to be quadratic in terms of
// spy_str_index/spy_str_concat calls.

#include <stdint.h>
#include <string.h>
#include "runtime.h"

#define SPY_DATA(s) ((const char*)((s) + sizeof(int64_t)))

char *spy_ospath__substr(const char *s, int64_t start, int64_t end) {
    int64_t len = spy_str_len(s);
    if (start < 0) start = 0;
    if (end > len) end = len;
    if (end <= start) return spy_str_new("", 0);
    return spy_str_new(SPY_DATA(s) + start, end - start);
}

// Last-occurrence search of a single byte. `ch` is a spy str whose first
// byte is the byte we're looking for. Returns the byte index, or -1 if
// absent. v1 treats `ch` as a single-byte ASCII character; multi-byte is
// the caller's problem until spython grows real unicode.
int64_t spy_ospath__rfind_char(const char *s, const char *ch) {
    int64_t len = spy_str_len(s);
    int64_t clen = spy_str_len(ch);
    if (clen == 0) return -1;
    const char *data = SPY_DATA(s);
    unsigned char target = (unsigned char)SPY_DATA(ch)[0];
    for (int64_t i = len - 1; i >= 0; i--) {
        if ((unsigned char)data[i] == target) return i;
    }
    return -1;
}
