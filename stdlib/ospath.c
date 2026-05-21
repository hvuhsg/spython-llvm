// stdlib/ospath.c — stat helpers for ospath.spy (getsize / getmtime).
// Path string manipulation now lives in pure .spy using native str methods.

#include <stdint.h>
#include <string.h>
#include <sys/stat.h>
#include "runtime.h"

#define SPY_DATA(s) ((const char*)((s) + sizeof(int64_t)))

// File size in bytes, or -1 if path can't be stat'd.
int64_t spy_ospath__stat_size(const char *path_spy) {
    int64_t len = spy_str_len(path_spy);
    char path[4096];
    if (len < 0 || (size_t)len + 1 > sizeof(path)) return -1;
    memcpy(path, SPY_DATA(path_spy), (size_t)len);
    path[len] = '\0';
    struct stat st;
    if (stat(path, &st) != 0) return -1;
    return (int64_t)st.st_size;
}

// Last-modification time in seconds since the epoch, or -1.0 on error.
double spy_ospath__stat_mtime(const char *path_spy) {
    int64_t len = spy_str_len(path_spy);
    char path[4096];
    if (len < 0 || (size_t)len + 1 > sizeof(path)) return -1.0;
    memcpy(path, SPY_DATA(path_spy), (size_t)len);
    path[len] = '\0';
    struct stat st;
    if (stat(path, &st) != 0) return -1.0;
    return (double)st.st_mtime;
}
