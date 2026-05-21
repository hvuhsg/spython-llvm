// stdlib/os.c — POSIX primitives behind stdlib/os.spy.
//
// Errno classification mirrors io.c (0 ok, 1 ENOENT, 2 EACCES, 3 EISDIR,
// 4 other) so the spy-side _raise_os_error pattern can be shared verbatim.

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <unistd.h>
#include <errno.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <dirent.h>
#include "runtime.h"

#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

static int spy_os_last_err = 0;

static void spy_os_record_errno(void) {
    switch (errno) {
        case 0:        spy_os_last_err = 0; return;
        case ENOENT:   spy_os_last_err = 1; return;
        case EACCES:   spy_os_last_err = 2; return;
        case EISDIR:   spy_os_last_err = 3; return;
        default:       spy_os_last_err = 4; return;
    }
}

int64_t spy_os__last_error_class(void) {
    return (int64_t)spy_os_last_err;
}

// Copy a spy str payload into a NUL-terminated C string in `out`.
// Returns 0 on success, -1 if the payload doesn't fit.
static int spy_os_to_cstr(const char *spy_buf, char *out, size_t out_cap) {
    int64_t len = spy_str_len(spy_buf);
    if (len < 0 || (size_t)len + 1 > out_cap) return -1;
    memcpy(out, SPY_STR_DATA(spy_buf), (size_t)len);
    out[len] = '\0';
    return 0;
}

// _getenv_or returns the env var, or the supplied default str when unset.
// The spy wrapper supplies "" as the default to mimic the old behaviour.
char *spy_os__getenv_or(const char *name_spy, const char *default_spy) {
    char name[1024];
    if (spy_os_to_cstr(name_spy, name, sizeof(name)) != 0) {
        spy_os_last_err = 4;
        return (char *)default_spy;
    }
    const char *val = getenv(name);
    if (val == NULL) return (char *)default_spy;
    return spy_str_new(val, (int64_t)strlen(val));
}

// urandom(n): n bytes from /dev/urandom (falls back to rand()).
char *spy_os_urandom(int64_t n) {
    if (n < 0) n = 0;
    unsigned char *buf = (unsigned char *)malloc((size_t)(n > 0 ? n : 1));
    FILE *f = fopen("/dev/urandom", "rb");
    size_t got = 0;
    if (f) {
        got = fread(buf, 1, (size_t)n, f);
        fclose(f);
    }
    for (size_t i = got; i < (size_t)n; i++) buf[i] = (unsigned char)(rand() & 0xff);
    char *r = spy_str_new((const char *)buf, n);
    free(buf);
    return r;
}

int64_t spy_os_system(const char *cmd_spy) {
    char cmd[8192];
    if (spy_os_to_cstr(cmd_spy, cmd, sizeof(cmd)) != 0) return -1;
    return (int64_t)system(cmd);
}

char *spy_os_strerror(int64_t code) {
    const char *msg = strerror((int)code);
    return spy_str_new(msg, (int64_t)strlen(msg));
}

char *spy_os_getcwd(void) {
    char buf[4096];
    if (getcwd(buf, sizeof(buf)) == NULL) {
        spy_os_record_errno();
        return spy_str_new("", 0);
    }
    spy_os_last_err = 0;
    return spy_str_new(buf, (int64_t)strlen(buf));
}

int64_t spy_os_getpid(void) {
    return (int64_t)getpid();
}

int64_t spy_os__chdir(const char *path_spy) {
    char path[4096];
    if (spy_os_to_cstr(path_spy, path, sizeof(path)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    if (chdir(path) != 0) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_last_err = 0;
    return 0;
}

int64_t spy_os__mkdir(const char *path_spy) {
    char path[4096];
    if (spy_os_to_cstr(path_spy, path, sizeof(path)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    if (mkdir(path, 0755) != 0) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_last_err = 0;
    return 0;
}

int64_t spy_os__remove(const char *path_spy) {
    char path[4096];
    if (spy_os_to_cstr(path_spy, path, sizeof(path)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    if (unlink(path) != 0) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_last_err = 0;
    return 0;
}

int64_t spy_os__rmdir(const char *path_spy) {
    char path[4096];
    if (spy_os_to_cstr(path_spy, path, sizeof(path)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    if (rmdir(path) != 0) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_last_err = 0;
    return 0;
}

int64_t spy_os__rename(const char *src_spy, const char *dst_spy) {
    char src[4096], dst[4096];
    if (spy_os_to_cstr(src_spy, src, sizeof(src)) != 0 ||
        spy_os_to_cstr(dst_spy, dst, sizeof(dst)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    if (rename(src, dst) != 0) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_last_err = 0;
    return 0;
}

int64_t spy_os__stat_mode(const char *path_spy) {
    char path[4096];
    if (spy_os_to_cstr(path_spy, path, sizeof(path)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    struct stat st;
    if (stat(path, &st) != 0) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_last_err = 0;
    return (int64_t)st.st_mode;
}

// Directory listing: populated by _listdir_read, read out by _listdir_entry.
// Single-caller state; not re-entrant (good enough for v1).
static char **spy_os_dir_entries = NULL;
static int64_t spy_os_dir_count = 0;
static int64_t spy_os_dir_cap = 0;

static void spy_os_dir_reset(void) {
    // Entries are heap-allocated C strings (strdup); free them between calls.
    // The spy_str copies made for return have their own GC lifetime, so we
    // don't touch those.
    for (int64_t i = 0; i < spy_os_dir_count; i++) {
        free(spy_os_dir_entries[i]);
    }
    spy_os_dir_count = 0;
}

int64_t spy_os__listdir_read(const char *path_spy) {
    char path[4096];
    if (spy_os_to_cstr(path_spy, path, sizeof(path)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    DIR *d = opendir(path);
    if (d == NULL) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_dir_reset();
    struct dirent *ent;
    while ((ent = readdir(d)) != NULL) {
        if (strcmp(ent->d_name, ".") == 0 || strcmp(ent->d_name, "..") == 0) continue;
        if (spy_os_dir_count == spy_os_dir_cap) {
            int64_t new_cap = spy_os_dir_cap == 0 ? 16 : spy_os_dir_cap * 2;
            char **new_entries = (char**)realloc(spy_os_dir_entries, (size_t)new_cap * sizeof(char*));
            if (new_entries == NULL) {
                closedir(d);
                spy_os_last_err = 4;
                return -1;
            }
            spy_os_dir_entries = new_entries;
            spy_os_dir_cap = new_cap;
        }
        spy_os_dir_entries[spy_os_dir_count++] = strdup(ent->d_name);
    }
    closedir(d);
    spy_os_last_err = 0;
    return spy_os_dir_count;
}

char *spy_os__listdir_entry(int64_t i) {
    if (i < 0 || i >= spy_os_dir_count) return spy_str_new("", 0);
    const char *s = spy_os_dir_entries[i];
    return spy_str_new(s, (int64_t)strlen(s));
}

// ----- Environment / process -----

int64_t spy_os__setenv(const char *name_spy, const char *value_spy) {
    char name[1024], value[4096];
    if (spy_os_to_cstr(name_spy, name, sizeof(name)) != 0 ||
        spy_os_to_cstr(value_spy, value, sizeof(value)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    if (setenv(name, value, 1) != 0) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_last_err = 0;
    return 0;
}

int64_t spy_os__unsetenv(const char *name_spy) {
    char name[1024];
    if (spy_os_to_cstr(name_spy, name, sizeof(name)) != 0) {
        spy_os_last_err = 4;
        return -1;
    }
    if (unsetenv(name) != 0) {
        spy_os_record_errno();
        return -1;
    }
    spy_os_last_err = 0;
    return 0;
}

// CPython's os.umask returns the previous mask and never raises.
int64_t spy_os_umask(int64_t mask) {
    mode_t prev = umask((mode_t)mask);
    return (int64_t)prev;
}

int64_t spy_os_cpu_count(void) {
    long n = sysconf(_SC_NPROCESSORS_ONLN);
    if (n <= 0) return 1;
    return (int64_t)n;
}

int64_t spy_os_access(const char *path_spy, int64_t mode) {
    char path[4096];
    if (spy_os_to_cstr(path_spy, path, sizeof(path)) != 0) {
        return 0;
    }
    return access(path, (int)mode) == 0 ? 1 : 0;
}
