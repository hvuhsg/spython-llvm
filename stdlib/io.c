// stdlib/io.c — POSIX-backed file I/O primitives for stdlib/io.spy.
//
// All length-prefixed values use the spy_str layout from runtime.h:
//   [int64_t len][data...]
// We treat str and bytes interchangeably at this boundary — the runtime
// layout is identical, the type system distinguishes them at compile time.

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>
#include <sys/stat.h>
#include <gc.h>
#include "runtime.h"

// Last-error classification, populated by every primitive that can fail.
// Codes match the order checked by io.spy's _last_error_class wrapper:
//   0 = ok, 1 = ENOENT, 2 = EACCES, 3 = EISDIR, 4 = other.
static int spy_io_last_err = 0;

static void spy_io_record_errno(void) {
    switch (errno) {
        case 0:        spy_io_last_err = 0; return;
        case ENOENT:   spy_io_last_err = 1; return;
        case EACCES:   spy_io_last_err = 2; return;
        case EISDIR:   spy_io_last_err = 3; return;
        default:       spy_io_last_err = 4; return;
    }
}

int64_t spy_io__last_error_class(void) {
    return (int64_t)spy_io_last_err;
}

// Copy a spy str/bytes payload into a NUL-terminated stack buffer for
// passing to POSIX APIs that need a C string. Returns 0 on success, -1 if
// the payload doesn't fit. Callers handle the failure by setting last_err
// and returning a sentinel.
static int spy_io_to_cstr(const char *spy_buf, char *out, size_t out_cap) {
    int64_t len = *(int64_t*)spy_buf;
    if (len < 0 || (size_t)len + 1 > out_cap) return -1;
    memcpy(out, spy_buf + sizeof(int64_t), (size_t)len);
    out[len] = '\0';
    return 0;
}

// Mode parser. Recognizes "r", "w", "a", and the binary variants "rb",
// "wb", "ab" (the 'b' is a no-op at this layer — str/bytes share runtime
// layout). Returns POSIX flags or -1 on unknown mode.
static int spy_io_parse_mode(const char *mode_spy) {
    int64_t len = *(int64_t*)mode_spy;
    const char *m = mode_spy + sizeof(int64_t);
    if (len < 1 || len > 2) return -1;
    char c = m[0];
    if (len == 2 && m[1] != 'b') return -1;
    switch (c) {
        case 'r': return O_RDONLY;
        case 'w': return O_WRONLY | O_CREAT | O_TRUNC;
        case 'a': return O_WRONLY | O_CREAT | O_APPEND;
        default:  return -1;
    }
}

int64_t spy_io__open_fd(const char *path_spy, const char *mode_spy) {
    char path[4096];
    if (spy_io_to_cstr(path_spy, path, sizeof(path)) != 0) {
        spy_io_last_err = 4;
        return -1;
    }
    int flags = spy_io_parse_mode(mode_spy);
    if (flags < 0) {
        spy_io_last_err = 4;
        return -1;
    }
    int fd = open(path, flags, 0644);
    if (fd < 0) {
        spy_io_record_errno();
        return -1;
    }
    spy_io_last_err = 0;
    return (int64_t)fd;
}

void spy_io__close_fd(int64_t fd) {
    if (fd < 0) return;
    close((int)fd);
}

// Read everything from current position to EOF. Grows a heap buffer
// geometrically; copies into a spy_str at the end so the caller gets a
// length-prefixed payload.
char *spy_io__read_all_fd(int64_t fd) {
    size_t cap = 4096;
    size_t len = 0;
    char *buf = (char *)GC_MALLOC_ATOMIC(cap);
    for (;;) {
        if (len == cap) {
            cap *= 2;
            buf = (char *)GC_REALLOC(buf, cap);
        }
        ssize_t n = read((int)fd, buf + len, cap - len);
        if (n < 0) {
            spy_io_record_errno();
            return spy_str_new("", 0);
        }
        if (n == 0) break;
        len += (size_t)n;
    }
    spy_io_last_err = 0;
    return spy_str_new(buf, (int64_t)len);
}

char *spy_io__read_n_fd(int64_t fd, int64_t n) {
    if (n < 0) return spy_io__read_all_fd(fd);
    if (n == 0) return spy_str_new("", 0);
    char *buf = (char *)GC_MALLOC_ATOMIC((size_t)n);
    size_t got = 0;
    while (got < (size_t)n) {
        ssize_t r = read((int)fd, buf + got, (size_t)n - got);
        if (r < 0) {
            spy_io_record_errno();
            return spy_str_new("", 0);
        }
        if (r == 0) break;
        got += (size_t)r;
    }
    spy_io_last_err = 0;
    return spy_str_new(buf, (int64_t)got);
}

// Read up to and including the next '\n'. Returns "" at EOF.
// Reads byte-at-a-time — fine for testing; can be buffered later if perf
// matters. Buffered readline would require a per-fd userspace buffer that
// shares state across calls, which is more machinery than the current
// stdlib pattern carries.
char *spy_io__readline_fd(int64_t fd) {
    size_t cap = 256;
    size_t len = 0;
    char *buf = (char *)GC_MALLOC_ATOMIC(cap);
    for (;;) {
        if (len == cap) {
            cap *= 2;
            buf = (char *)GC_REALLOC(buf, cap);
        }
        char c;
        ssize_t r = read((int)fd, &c, 1);
        if (r < 0) {
            spy_io_record_errno();
            return spy_str_new("", 0);
        }
        if (r == 0) break;
        buf[len++] = c;
        if (c == '\n') break;
    }
    spy_io_last_err = 0;
    return spy_str_new(buf, (int64_t)len);
}

int64_t spy_io__write_fd(int64_t fd, const char *data_spy) {
    int64_t len = *(int64_t*)data_spy;
    const char *data = data_spy + sizeof(int64_t);
    size_t written = 0;
    while (written < (size_t)len) {
        ssize_t w = write((int)fd, data + written, (size_t)len - written);
        if (w < 0) {
            spy_io_record_errno();
            return -1;
        }
        written += (size_t)w;
    }
    spy_io_last_err = 0;
    return (int64_t)written;
}

int64_t spy_io__tell_fd(int64_t fd) {
    off_t pos = lseek((int)fd, 0, SEEK_CUR);
    if (pos < 0) {
        spy_io_record_errno();
        return -1;
    }
    spy_io_last_err = 0;
    return (int64_t)pos;
}

// whence: 0=SEEK_SET, 1=SEEK_CUR, 2=SEEK_END (matches Python's os module).
int64_t spy_io__seek_fd(int64_t fd, int64_t offset, int64_t whence) {
    int w;
    switch (whence) {
        case 0: w = SEEK_SET; break;
        case 1: w = SEEK_CUR; break;
        case 2: w = SEEK_END; break;
        default:
            spy_io_last_err = 4;
            return -1;
    }
    off_t pos = lseek((int)fd, (off_t)offset, w);
    if (pos < 0) {
        spy_io_record_errno();
        return -1;
    }
    spy_io_last_err = 0;
    return (int64_t)pos;
}
