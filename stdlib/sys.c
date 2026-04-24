// stdlib/sys.c — process-level primitives for stdlib/sys.spy.
//
// argv is stashed by the runtime (see spy_argv_set in runtime.c, which
// codegen calls at the top of main()). Everything else is a thin wrapper
// over the standard C library.

#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include "runtime.h"

#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

int64_t spy_sys__argc(void) {
    return (int64_t)spy_argv_count();
}

char *spy_sys__argv_at(int64_t i) {
    const char *s = spy_argv_at((int)i);
    return spy_str_new(s, (int64_t)strlen(s));
}

void spy_sys_exit(int64_t code) {
    exit((int)code);
}

char *spy_sys_platform(void) {
#if defined(__APPLE__)
    const char *p = "darwin";
#elif defined(__linux__)
    const char *p = "linux";
#elif defined(_WIN32)
    const char *p = "win32";
#else
    const char *p = "unknown";
#endif
    return spy_str_new(p, (int64_t)strlen(p));
}

void spy_sys_stdout_write(const char *s) {
    int64_t n = spy_str_len(s);
    fwrite(SPY_STR_DATA(s), 1, (size_t)n, stdout);
}

void spy_sys_stderr_write(const char *s) {
    int64_t n = spy_str_len(s);
    fwrite(SPY_STR_DATA(s), 1, (size_t)n, stderr);
}
