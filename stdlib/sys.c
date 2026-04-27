// stdlib/sys.c — process-level primitives for stdlib/sys.spy.
//
// argv is stashed by the runtime (see spy_argv_set in runtime.c, which
// codegen calls at the top of main()). Everything else is a thin wrapper
// over the standard C library.

#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <limits.h>
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

char *spy_sys__platform(void) {
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

// CPython's sys.byteorder: "little" or "big". We probe at runtime so we
// don't rely on a build-time macro that varies across libcs.
char *spy_sys__byteorder(void) {
    uint16_t one = 1;
    const char *p = (*(uint8_t*)&one == 1) ? "little" : "big";
    return spy_str_new(p, (int64_t)strlen(p));
}

// sys.maxsize: largest signed Py_ssize_t. spython's int is i64 across all
// platforms, so this is INT64_MAX uniformly — slightly different from
// CPython on 32-bit hosts, but spython doesn't target those.
int64_t spy_sys__maxsize(void) {
    return INT64_MAX;
}

// sys.version: spython's own version string, formatted to *resemble*
// CPython's (e.g. "spython 0.1 [arm64]"). It is *not* a CPython release
// string; users discriminating in code should check sys.platform plus a
// custom version probe instead.
char *spy_sys__version(void) {
    const char *v = "spython 0.1";
    return spy_str_new(v, (int64_t)strlen(v));
}

// sys.executable: argv[0] is a reasonable approximation when nothing more
// authoritative is available. CPython resolves the real interpreter path;
// spython compiles to a binary, so argv[0] *is* the executable.
char *spy_sys__executable(void) {
    if (spy_argv_count() == 0) return spy_str_new("", 0);
    const char *s = spy_argv_at(0);
    return spy_str_new(s, (int64_t)strlen(s));
}

void spy_sys_stdout_write(const char *s) {
    int64_t n = spy_str_len(s);
    fwrite(SPY_STR_DATA(s), 1, (size_t)n, stdout);
}

void spy_sys_stderr_write(const char *s) {
    int64_t n = spy_str_len(s);
    fwrite(SPY_STR_DATA(s), 1, (size_t)n, stderr);
}
