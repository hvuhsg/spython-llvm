// stdlib/time.c — portable wrappers over POSIX time APIs.

#include <time.h>
#include <errno.h>

// Seconds since the Unix epoch, with nanosecond resolution.
double spy_time_time(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

// A monotonic clock suitable for interval measurement.
double spy_time_monotonic(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
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
