// stdlib/time.c — portable wrappers over POSIX time APIs.

#include <time.h>
#include <errno.h>
#include <stdint.h>
#include <sys/resource.h>

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

void spy_time_sleep(double seconds) {
    if (seconds <= 0.0) return;
    struct timespec req;
    req.tv_sec = (time_t)seconds;
    req.tv_nsec = (long)((seconds - (double)req.tv_sec) * 1e9);
    // Resume on EINTR so signal delivery doesn't cut the sleep short.
    while (nanosleep(&req, &req) == -1 && errno == EINTR) {
    }
}
