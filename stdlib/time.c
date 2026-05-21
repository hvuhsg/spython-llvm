// stdlib/time.c — portable wrappers over POSIX time APIs.

#include <time.h>
#include <errno.h>
#include <stdint.h>
#include <string.h>
#include <sys/resource.h>
#include "runtime.h"

#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

// Seconds since the Unix epoch, with nanosecond resolution.
double spy_time_time(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

int64_t spy_time_time_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (int64_t)ts.tv_sec * 1000000000LL + (int64_t)ts.tv_nsec;
}

// A monotonic clock suitable for interval measurement.
double spy_time_monotonic(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

int64_t spy_time_monotonic_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000000LL + (int64_t)ts.tv_nsec;
}

// perf_counter on POSIX is documented as the highest-resolution monotonic
// clock — same source as monotonic() in practice.
double spy_time_perf_counter(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

int64_t spy_time_perf_counter_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000000LL + (int64_t)ts.tv_nsec;
}

// process_time: user+system CPU consumed by the process (excluding sleep).
// CLOCK_PROCESS_CPUTIME_ID is preferred when available; we fall back to
// getrusage on platforms where it isn't (notably some older macOS releases).
static int process_cpu_time_ns(int64_t *out) {
#ifdef CLOCK_PROCESS_CPUTIME_ID
    struct timespec ts;
    if (clock_gettime(CLOCK_PROCESS_CPUTIME_ID, &ts) == 0) {
        *out = (int64_t)ts.tv_sec * 1000000000LL + (int64_t)ts.tv_nsec;
        return 0;
    }
#endif
    struct rusage ru;
    if (getrusage(RUSAGE_SELF, &ru) == 0) {
        int64_t u = (int64_t)ru.ru_utime.tv_sec * 1000000000LL + (int64_t)ru.ru_utime.tv_usec * 1000LL;
        int64_t s = (int64_t)ru.ru_stime.tv_sec * 1000000000LL + (int64_t)ru.ru_stime.tv_usec * 1000LL;
        *out = u + s;
        return 0;
    }
    return -1;
}

double spy_time_process_time(void) {
    int64_t ns = 0;
    process_cpu_time_ns(&ns);
    return (double)ns / 1e9;
}

int64_t spy_time_process_time_ns(void) {
    int64_t ns = 0;
    process_cpu_time_ns(&ns);
    return ns;
}

// ----- Calendar (struct_time) support -----
//
// A static struct tm is loaded by _load_local / _load_gm and read field by
// field via _tm_field, hiding the broken-down-time struct behind the FFI.
// Field indices match the spy struct_time constructor order. Conversions to
// Python conventions: full year, 1-based month, 1-based yday, and Monday=0
// weekday.

static struct tm g_tm;

int64_t spy_time__load_local(int64_t t) {
    time_t tt = (time_t)t;
    localtime_r(&tt, &g_tm);
    return 0;
}

int64_t spy_time__load_gm(int64_t t) {
    time_t tt = (time_t)t;
    gmtime_r(&tt, &g_tm);
    return 0;
}

int64_t spy_time__now(void) {
    return (int64_t)time(NULL);
}

int64_t spy_time__tm_field(int64_t which) {
    switch (which) {
        case 0: return (int64_t)g_tm.tm_year + 1900;
        case 1: return (int64_t)g_tm.tm_mon + 1;
        case 2: return (int64_t)g_tm.tm_mday;
        case 3: return (int64_t)g_tm.tm_hour;
        case 4: return (int64_t)g_tm.tm_min;
        case 5: return (int64_t)g_tm.tm_sec;
        case 6: return (int64_t)((g_tm.tm_wday + 6) % 7); // Mon=0..Sun=6
        case 7: return (int64_t)g_tm.tm_yday + 1;
        case 8: return (int64_t)g_tm.tm_isdst;
    }
    return 0;
}

// Build a struct tm from Python-convention fields (inverse of _tm_field).
static void fill_tm(struct tm *tm, int64_t year, int64_t mon, int64_t mday,
                    int64_t hour, int64_t minute, int64_t sec,
                    int64_t wday, int64_t yday, int64_t isdst) {
    memset(tm, 0, sizeof(*tm));
    tm->tm_year = (int)(year - 1900);
    tm->tm_mon = (int)(mon - 1);
    tm->tm_mday = (int)mday;
    tm->tm_hour = (int)hour;
    tm->tm_min = (int)minute;
    tm->tm_sec = (int)sec;
    tm->tm_wday = (int)((wday + 1) % 7); // Mon=0..Sun=6 -> Sun=0..Sat=6
    tm->tm_yday = (int)(yday - 1);
    tm->tm_isdst = (int)isdst;
}

int64_t spy_time__mktime(int64_t year, int64_t mon, int64_t mday, int64_t hour,
                         int64_t minute, int64_t sec, int64_t wday,
                         int64_t yday, int64_t isdst) {
    struct tm tm;
    fill_tm(&tm, year, mon, mday, hour, minute, sec, wday, yday, isdst);
    return (int64_t)mktime(&tm);
}

char *spy_time__strftime(const char *fmt_spy, int64_t year, int64_t mon,
                         int64_t mday, int64_t hour, int64_t minute,
                         int64_t sec, int64_t wday, int64_t yday,
                         int64_t isdst) {
    char fmt[1024];
    int64_t flen = spy_str_len(fmt_spy);
    if (flen < 0 || (size_t)flen + 1 > sizeof(fmt)) return spy_str_new("", 0);
    memcpy(fmt, SPY_STR_DATA(fmt_spy), (size_t)flen);
    fmt[flen] = '\0';
    struct tm tm;
    fill_tm(&tm, year, mon, mday, hour, minute, sec, wday, yday, isdst);
    char out[4096];
    size_t n = strftime(out, sizeof(out), fmt, &tm);
    return spy_str_new(out, (int64_t)n);
}

char *spy_time__asctime(int64_t year, int64_t mon, int64_t mday, int64_t hour,
                        int64_t minute, int64_t sec, int64_t wday,
                        int64_t yday, int64_t isdst) {
    struct tm tm;
    fill_tm(&tm, year, mon, mday, hour, minute, sec, wday, yday, isdst);
    char buf[64];
    // asctime_r writes 26 bytes including a trailing newline; drop it to
    // match CPython's time.asctime (which has no trailing newline).
    if (asctime_r(&tm, buf) == NULL) return spy_str_new("", 0);
    size_t len = strlen(buf);
    if (len > 0 && buf[len - 1] == '\n') len--;
    return spy_str_new(buf, (int64_t)len);
}

void spy_time_sleep(double seconds) {
    if (seconds <= 0.0) return;
    struct timespec req;
    req.tv_sec = (time_t)seconds;
    req.tv_nsec = (long)((seconds - (double)req.tv_sec) * 1e9);
    // Resume on EINTR so signal delivery doesn't cut the sleep short.
    while (nanosleep(&req, &req) == -1 && errno == EINTR) {
    }
}
