// spython-link: -lpthread
//
// stdlib/socket.c — POSIX-backed socket primitives for stdlib/socket.spy.
// Scope: IPv4 (AF_INET) with TCP (SOCK_STREAM) or UDP (SOCK_DGRAM).
// Both client (connect) and server (bind/listen/accept) sides are
// supported. Sockets are blocking unless set otherwise via setblocking()
// or settimeout().
//
// The str/bytes ABI is the same shared layout as io.c: a char* pointing at
// [int64_t len][data...]. send() reads that layout directly; recv() / the
// "last peer" stash use spy_str_new so the caller receives proper spython
// values.

#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include <errno.h>
#include <pthread.h>
#include <time.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/select.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <gc.h>
#include "runtime.h"

// Last-error classification, populated by every primitive that can fail.
// Decoded by socket.spy's _last_error_class wrapper:
//   0 ok, 1 ECONNREFUSED, 2 generic, 3 timeout (ETIMEDOUT or EAGAIN with
//   timeout set), 4 ECONNRESET, 5 ECONNABORTED, 6 EPIPE,
//   7 EAGAIN/EWOULDBLOCK without timeout, 8 EAI_* (resolver failure).
static int spy_socket_last_err = 0;

// Per-fd timeout flag. settimeout() flips it on for fds that have a
// non-blocking-with-timeout configuration, so EAGAIN/EWOULDBLOCK on those
// fds gets reported as TimeoutError instead of BlockingIOError. We index
// by fd because the Socket object is not passed back through the C ABI.
// FDs above this cap fall back to "no timeout configured", which is
// merely a classification fidelity issue, not a correctness one.
#define SPY_SOCKET_FD_CAP 4096
static unsigned char spy_socket_has_timeout[SPY_SOCKET_FD_CAP];
// Recorded settimeout() value in seconds, parallel to has_timeout. Read
// by connect() when has_timeout is set so the connect phase honours the
// same deadline as recv/send. SO_RCVTIMEO / SO_SNDTIMEO themselves do
// not affect connect() — that's why we keep this slot.
static double spy_socket_timeout_secs[SPY_SOCKET_FD_CAP];

// Stash for accept/recvfrom/getsockname/getpeername peer addresses.
// The spython side reads them via _last_peer_host()/_last_peer_port()
// immediately after the call that produced them.
static char *spy_socket_peer_host = NULL;  // owned by Boehm GC (spy_str_new)
static int64_t spy_socket_peer_port = 0;

static void spy_socket_clear_peer(void) {
    spy_socket_peer_host = spy_str_new("", 0);
    spy_socket_peer_port = 0;
}

static void spy_socket_record_errno(void) {
    int e = errno;
    switch (e) {
        case 0:             spy_socket_last_err = 0; return;
        case ECONNREFUSED:  spy_socket_last_err = 1; return;
        case ETIMEDOUT:     spy_socket_last_err = 3; return;
        case ECONNRESET:    spy_socket_last_err = 4; return;
        case ECONNABORTED:  spy_socket_last_err = 5; return;
        case EPIPE:         spy_socket_last_err = 6; return;
        case EAGAIN:
#if EWOULDBLOCK != EAGAIN
        case EWOULDBLOCK:
#endif
            // EAGAIN means "timed out" only when the fd had a timeout
            // installed; otherwise it's the non-blocking "would block"
            // signal that maps to BlockingIOError.
            spy_socket_last_err = 7;
            return;
        default:            spy_socket_last_err = 2; return;
    }
}

static void spy_socket_record_with_timeout_check(int fd) {
    int e = errno;
    if (e == ETIMEDOUT) {
        spy_socket_last_err = 3;
        return;
    }
    if (e == EAGAIN
#if EWOULDBLOCK != EAGAIN
        || e == EWOULDBLOCK
#endif
    ) {
        if (fd >= 0 && fd < SPY_SOCKET_FD_CAP && spy_socket_has_timeout[fd]) {
            spy_socket_last_err = 3;
            return;
        }
        spy_socket_last_err = 7;
        return;
    }
    spy_socket_record_errno();
}

int64_t spy_socket__last_error_class(void) {
    return (int64_t)spy_socket_last_err;
}

// Decode a spython-side str pointer into a NUL-terminated C buffer.
// Returns the C string in `out` (caller-provided, size `cap`). Returns 0
// on success, -1 if the host length is invalid for the buffer.
static int spy_socket_str_to_c(const char *spy_str, char *out, size_t cap) {
    if (spy_str == NULL) return -1;
    int64_t len = *(int64_t*)spy_str;
    if (len < 0 || (size_t)len >= cap) return -1;
    memcpy(out, spy_str + sizeof(int64_t), (size_t)len);
    out[len] = '\0';
    return 0;
}

// Resolve `host` (numeric IPv4 first, then DNS) into addr.sin_addr. Sets
// the error class on failure.
static int spy_socket_resolve_inet(const char *host_c, struct sockaddr_in *addr) {
    if (inet_pton(AF_INET, host_c, &addr->sin_addr) == 1) return 0;
    struct addrinfo hints;
    struct addrinfo *res = NULL;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_INET;
    int gai = getaddrinfo(host_c, NULL, &hints, &res);
    if (gai != 0 || res == NULL) {
        spy_socket_last_err = 8;
        if (res) freeaddrinfo(res);
        return -1;
    }
    struct sockaddr_in *rin = (struct sockaddr_in *)res->ai_addr;
    addr->sin_addr = rin->sin_addr;
    freeaddrinfo(res);
    return 0;
}

// Stash a sockaddr_in into the peer slots so spython can read it back.
static void spy_socket_stash_peer(const struct sockaddr_in *addr) {
    char buf[INET_ADDRSTRLEN];
    if (inet_ntop(AF_INET, &addr->sin_addr, buf, sizeof(buf)) == NULL) {
        spy_socket_peer_host = spy_str_new("", 0);
    } else {
        spy_socket_peer_host = spy_str_new(buf, (int64_t)strlen(buf));
    }
    spy_socket_peer_port = (int64_t)ntohs(addr->sin_port);
}

int64_t spy_socket__socket(int64_t family, int64_t type) {
    int fd = socket((int)family, (int)type, 0);
    if (fd < 0) {
        spy_socket_record_errno();
        return -1;
    }
    if (fd >= 0 && fd < SPY_SOCKET_FD_CAP) spy_socket_has_timeout[fd] = 0;
    spy_socket_last_err = 0;
    return (int64_t)fd;
}

int64_t spy_socket__connect(int64_t fd, const char *host_spy, int64_t port) {
    char host[256];
    if (spy_socket_str_to_c(host_spy, host, sizeof(host)) < 0) {
        spy_socket_last_err = 2;
        return -1;
    }

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons((uint16_t)port);
    if (spy_socket_resolve_inet(host, &addr) < 0) return -1;

    int ifd = (int)fd;
    int has_to = (ifd >= 0 && ifd < SPY_SOCKET_FD_CAP && spy_socket_has_timeout[ifd]);
    if (!has_to) {
        // No timeout configured: keep the historical fully-blocking
        // semantics. SO_RCVTIMEO / SO_SNDTIMEO do not affect connect, so
        // a blocking connect can still hang here — callers that need a
        // bounded connect must call settimeout() before connect().
        int rc = connect(ifd, (struct sockaddr *)&addr, sizeof(addr));
        if (rc < 0) {
            spy_socket_record_with_timeout_check(ifd);
            return -1;
        }
        spy_socket_last_err = 0;
        return 0;
    }

    // Timeout-bounded connect. Switch to non-blocking, start the
    // connection, then select() until it completes or the deadline
    // fires. Restore the original blocking flag at the end so subsequent
    // recv/send keep using SO_RCVTIMEO/SO_SNDTIMEO.
    int orig_flags = fcntl(ifd, F_GETFL, 0);
    if (orig_flags < 0) {
        spy_socket_record_errno();
        return -1;
    }
    if (fcntl(ifd, F_SETFL, orig_flags | O_NONBLOCK) < 0) {
        spy_socket_record_errno();
        return -1;
    }

    int rc = connect(ifd, (struct sockaddr *)&addr, sizeof(addr));
    if (rc < 0 && errno != EINPROGRESS) {
        spy_socket_record_with_timeout_check(ifd);
        fcntl(ifd, F_SETFL, orig_flags);
        return -1;
    }

    if (rc != 0) {
        // EINPROGRESS: wait for writability with our deadline.
        double secs = spy_socket_timeout_secs[ifd];
        struct timeval tv;
        tv.tv_sec = (time_t)secs;
        tv.tv_usec = (suseconds_t)((secs - (double)tv.tv_sec) * 1000000.0);
        fd_set wfds;
        FD_ZERO(&wfds);
        FD_SET(ifd, &wfds);
        int sel = select(ifd + 1, NULL, &wfds, NULL, &tv);
        if (sel == 0) {
            spy_socket_last_err = 3; // timeout
            fcntl(ifd, F_SETFL, orig_flags);
            return -1;
        }
        if (sel < 0) {
            spy_socket_record_errno();
            fcntl(ifd, F_SETFL, orig_flags);
            return -1;
        }
        // Connection finished — check SO_ERROR to find out how.
        int soerr = 0;
        socklen_t slen = sizeof(soerr);
        if (getsockopt(ifd, SOL_SOCKET, SO_ERROR, &soerr, &slen) < 0) {
            spy_socket_record_errno();
            fcntl(ifd, F_SETFL, orig_flags);
            return -1;
        }
        if (soerr != 0) {
            errno = soerr;
            spy_socket_record_with_timeout_check(ifd);
            fcntl(ifd, F_SETFL, orig_flags);
            return -1;
        }
    }

    fcntl(ifd, F_SETFL, orig_flags);
    spy_socket_last_err = 0;
    return 0;
}

int64_t spy_socket__bind(int64_t fd, const char *host_spy, int64_t port) {
    char host[256];
    if (spy_socket_str_to_c(host_spy, host, sizeof(host)) < 0) {
        spy_socket_last_err = 2;
        return -1;
    }

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons((uint16_t)port);

    // "" or "0.0.0.0" -> INADDR_ANY. Otherwise resolve.
    int64_t hlen = *(int64_t*)host_spy;
    if (hlen == 0) {
        addr.sin_addr.s_addr = htonl(INADDR_ANY);
    } else if (spy_socket_resolve_inet(host, &addr) < 0) {
        return -1;
    }

    int rc = bind((int)fd, (struct sockaddr *)&addr, sizeof(addr));
    if (rc < 0) {
        spy_socket_record_errno();
        return -1;
    }
    spy_socket_last_err = 0;
    return 0;
}

int64_t spy_socket__listen(int64_t fd, int64_t backlog) {
    int rc = listen((int)fd, (int)backlog);
    if (rc < 0) {
        spy_socket_record_errno();
        return -1;
    }
    spy_socket_last_err = 0;
    return 0;
}

int64_t spy_socket__accept(int64_t fd) {
    struct sockaddr_in peer;
    socklen_t plen = sizeof(peer);
    int new_fd;
    for (;;) {
        new_fd = accept((int)fd, (struct sockaddr *)&peer, &plen);
        if (new_fd < 0 && errno == EINTR) continue;
        break;
    }
    if (new_fd < 0) {
        spy_socket_record_with_timeout_check((int)fd);
        spy_socket_clear_peer();
        return -1;
    }
    if (new_fd < SPY_SOCKET_FD_CAP) spy_socket_has_timeout[new_fd] = 0;
    spy_socket_stash_peer(&peer);
    spy_socket_last_err = 0;
    return (int64_t)new_fd;
}

int64_t spy_socket__shutdown(int64_t fd, int64_t how) {
    int rc = shutdown((int)fd, (int)how);
    if (rc < 0) {
        spy_socket_record_errno();
        return -1;
    }
    spy_socket_last_err = 0;
    return 0;
}

int64_t spy_socket__send(int64_t fd, const char *data_spy) {
    int64_t len = *(int64_t*)data_spy;
    const char *buf = data_spy + sizeof(int64_t);
    int64_t total = 0;
    while (total < len) {
        ssize_t n = send((int)fd, buf + total, (size_t)(len - total), 0);
        if (n < 0) {
            if (errno == EINTR) continue;
            spy_socket_record_with_timeout_check((int)fd);
            return -1;
        }
        total += n;
    }
    spy_socket_last_err = 0;
    return total;
}

// Recv up to n bytes. On error, records errno and returns an empty bytes;
// callers must check _last_error_class() after every recv to distinguish
// "clean EOF / short read" from "error". n < 0 is treated as 0.
char *spy_socket__recv(int64_t fd, int64_t n) {
    if (n < 0) n = 0;
    size_t cap = (size_t)(n > 0 ? n : 1);
    char *tmp = (char *)malloc(cap);
    ssize_t got = 0;
    for (;;) {
        got = recv((int)fd, tmp, (size_t)n, 0);
        if (got < 0 && errno == EINTR) continue;
        break;
    }
    if (got < 0) {
        spy_socket_record_with_timeout_check((int)fd);
        got = 0;
    } else {
        spy_socket_last_err = 0;
    }
    char *out = spy_str_new(tmp, (int64_t)got);
    free(tmp);
    return out;
}

int64_t spy_socket__sendto(int64_t fd, const char *data_spy,
                           const char *host_spy, int64_t port) {
    char host[256];
    if (spy_socket_str_to_c(host_spy, host, sizeof(host)) < 0) {
        spy_socket_last_err = 2;
        return -1;
    }
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons((uint16_t)port);
    if (spy_socket_resolve_inet(host, &addr) < 0) return -1;

    int64_t len = *(int64_t*)data_spy;
    const char *buf = data_spy + sizeof(int64_t);
    ssize_t n;
    for (;;) {
        n = sendto((int)fd, buf, (size_t)len, 0,
                   (struct sockaddr *)&addr, sizeof(addr));
        if (n < 0 && errno == EINTR) continue;
        break;
    }
    if (n < 0) {
        spy_socket_record_with_timeout_check((int)fd);
        return -1;
    }
    spy_socket_last_err = 0;
    return (int64_t)n;
}

char *spy_socket__recvfrom(int64_t fd, int64_t n) {
    if (n < 0) n = 0;
    size_t cap = (size_t)(n > 0 ? n : 1);
    char *tmp = (char *)malloc(cap);
    struct sockaddr_in peer;
    socklen_t plen = sizeof(peer);
    ssize_t got = 0;
    for (;;) {
        got = recvfrom((int)fd, tmp, (size_t)n, 0,
                       (struct sockaddr *)&peer, &plen);
        if (got < 0 && errno == EINTR) continue;
        break;
    }
    if (got < 0) {
        spy_socket_record_with_timeout_check((int)fd);
        spy_socket_clear_peer();
        got = 0;
    } else {
        spy_socket_stash_peer(&peer);
        spy_socket_last_err = 0;
    }
    char *out = spy_str_new(tmp, (int64_t)got);
    free(tmp);
    return out;
}

int64_t spy_socket__getsockname(int64_t fd) {
    struct sockaddr_in addr;
    socklen_t alen = sizeof(addr);
    if (getsockname((int)fd, (struct sockaddr *)&addr, &alen) < 0) {
        spy_socket_record_errno();
        spy_socket_clear_peer();
        return -1;
    }
    spy_socket_stash_peer(&addr);
    spy_socket_last_err = 0;
    return 0;
}

int64_t spy_socket__getpeername(int64_t fd) {
    struct sockaddr_in addr;
    socklen_t alen = sizeof(addr);
    if (getpeername((int)fd, (struct sockaddr *)&addr, &alen) < 0) {
        spy_socket_record_errno();
        spy_socket_clear_peer();
        return -1;
    }
    spy_socket_stash_peer(&addr);
    spy_socket_last_err = 0;
    return 0;
}

const char *spy_socket__last_peer_host(void) {
    if (spy_socket_peer_host == NULL) spy_socket_clear_peer();
    return spy_socket_peer_host;
}

int64_t spy_socket__last_peer_port(void) {
    return spy_socket_peer_port;
}

// Map spython's stable SO_* indices onto the host's real values. Done
// here (not in spython code) so the integers exposed to spython remain
// portable across BSD/Linux. level==SOL_SOCKET is the only level handled
// today; passing a different level forwards the optname unchanged so
// users can plug raw IPPROTO_TCP/TCP_NODELAY combos through if needed.
static int spy_socket_resolve_optname(int64_t level, int64_t optname, int *out_level, int *out_name) {
    if (level == 1 /* SOL_SOCKET */) {
        *out_level = SOL_SOCKET;
        switch (optname) {
            case 1:  *out_name = SO_REUSEADDR; return 0;
            case 2:  *out_name = SO_KEEPALIVE; return 0;
            case 3:  *out_name = SO_BROADCAST; return 0;
            case 4:  *out_name = SO_RCVTIMEO;  return 0;
            case 5:  *out_name = SO_SNDTIMEO;  return 0;
            case 6:  *out_name = SO_RCVBUF;    return 0;
            case 7:  *out_name = SO_SNDBUF;    return 0;
            case 8:  *out_name = SO_ERROR;     return 0;
            case 9:  *out_name = SO_TYPE;      return 0;
#ifdef SO_REUSEPORT
            case 10: *out_name = SO_REUSEPORT; return 0;
#endif
            default: return -1;
        }
    }
    *out_level = (int)level;
    *out_name = (int)optname;
    return 0;
}

int64_t spy_socket__setsockopt_int(int64_t fd, int64_t level, int64_t optname, int64_t value) {
    int lvl, name;
    if (spy_socket_resolve_optname(level, optname, &lvl, &name) < 0) {
        spy_socket_last_err = 2;
        return -1;
    }
    int v = (int)value;
    if (setsockopt((int)fd, lvl, name, &v, sizeof(v)) < 0) {
        spy_socket_record_errno();
        return -1;
    }
    spy_socket_last_err = 0;
    return 0;
}

int64_t spy_socket__getsockopt_int(int64_t fd, int64_t level, int64_t optname) {
    int lvl, name;
    if (spy_socket_resolve_optname(level, optname, &lvl, &name) < 0) {
        spy_socket_last_err = 2;
        return 0;
    }
    int v = 0;
    socklen_t vlen = sizeof(v);
    if (getsockopt((int)fd, lvl, name, &v, &vlen) < 0) {
        spy_socket_record_errno();
        return 0;
    }
    spy_socket_last_err = 0;
    return (int64_t)v;
}

int64_t spy_socket__setblocking(int64_t fd, int64_t flag) {
    int f = fcntl((int)fd, F_GETFL, 0);
    if (f < 0) {
        spy_socket_record_errno();
        return -1;
    }
    if (flag) {
        f &= ~O_NONBLOCK;
    } else {
        f |= O_NONBLOCK;
    }
    if (fcntl((int)fd, F_SETFL, f) < 0) {
        spy_socket_record_errno();
        return -1;
    }
    if (fd >= 0 && fd < SPY_SOCKET_FD_CAP) spy_socket_has_timeout[(int)fd] = 0;
    spy_socket_last_err = 0;
    return 0;
}

int64_t spy_socket__settimeout(int64_t fd, double seconds) {
    int f = fcntl((int)fd, F_GETFL, 0);
    if (f < 0) {
        spy_socket_record_errno();
        return -1;
    }

    if (seconds < 0.0) {
        // Disable: revert to plain blocking mode, clear timeouts.
        f &= ~O_NONBLOCK;
        if (fcntl((int)fd, F_SETFL, f) < 0) {
            spy_socket_record_errno();
            return -1;
        }
        struct timeval zero = {0, 0};
        setsockopt((int)fd, SOL_SOCKET, SO_RCVTIMEO, &zero, sizeof(zero));
        setsockopt((int)fd, SOL_SOCKET, SO_SNDTIMEO, &zero, sizeof(zero));
        if (fd >= 0 && fd < SPY_SOCKET_FD_CAP) spy_socket_has_timeout[(int)fd] = 0;
    } else if (seconds == 0.0) {
        // Non-blocking.
        f |= O_NONBLOCK;
        if (fcntl((int)fd, F_SETFL, f) < 0) {
            spy_socket_record_errno();
            return -1;
        }
        if (fd >= 0 && fd < SPY_SOCKET_FD_CAP) spy_socket_has_timeout[(int)fd] = 0;
    } else {
        // Blocking with timeout: clear O_NONBLOCK, install SO_RCVTIMEO /
        // SO_SNDTIMEO. Mark the fd so we report ETIMEDOUT/EAGAIN as
        // TimeoutError rather than BlockingIOError. Stash the seconds
        // value so a subsequent connect() can apply the same deadline
        // (SO_RCVTIMEO/SO_SNDTIMEO don't affect connect).
        f &= ~O_NONBLOCK;
        if (fcntl((int)fd, F_SETFL, f) < 0) {
            spy_socket_record_errno();
            return -1;
        }
        struct timeval tv;
        tv.tv_sec = (time_t)seconds;
        tv.tv_usec = (suseconds_t)((seconds - (double)tv.tv_sec) * 1000000.0);
        if (setsockopt((int)fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv)) < 0) {
            spy_socket_record_errno();
            return -1;
        }
        if (setsockopt((int)fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv)) < 0) {
            spy_socket_record_errno();
            return -1;
        }
        if (fd >= 0 && fd < SPY_SOCKET_FD_CAP) {
            spy_socket_has_timeout[(int)fd] = 1;
            spy_socket_timeout_secs[(int)fd] = seconds;
        }
    }

    spy_socket_last_err = 0;
    return 0;
}

void spy_socket__close(int64_t fd) {
    if (fd < 0) return;
    if (fd < SPY_SOCKET_FD_CAP) spy_socket_has_timeout[(int)fd] = 0;
    close((int)fd);
}

const char *spy_socket__gethostname(void) {
    char buf[256];
    if (gethostname(buf, sizeof(buf)) < 0) {
        spy_socket_record_errno();
        return spy_str_new("", 0);
    }
    buf[sizeof(buf) - 1] = '\0';
    spy_socket_last_err = 0;
    return spy_str_new(buf, (int64_t)strlen(buf));
}

const char *spy_socket__gethostbyname(const char *host_spy) {
    char host[256];
    if (spy_socket_str_to_c(host_spy, host, sizeof(host)) < 0) {
        spy_socket_last_err = 2;
        return spy_str_new("", 0);
    }
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    if (spy_socket_resolve_inet(host, &addr) < 0) {
        return spy_str_new("", 0);
    }
    char out[INET_ADDRSTRLEN];
    if (inet_ntop(AF_INET, &addr.sin_addr, out, sizeof(out)) == NULL) {
        spy_socket_last_err = 2;
        return spy_str_new("", 0);
    }
    spy_socket_last_err = 0;
    return spy_str_new(out, (int64_t)strlen(out));
}

// Background DNS lookup with a deadline. getaddrinfo is blocking and
// uncancellable from a single thread, so callers that need a bounded
// resolver use this variant. We spawn a worker thread that runs
// getaddrinfo, then poll for completion until the deadline. On timeout
// the worker is detached and leaked — its eventual completion is
// harmless since the cleanup path re-checks the abandoned flag before
// touching the result struct.
typedef struct {
    char host[256];
    char ip[INET_ADDRSTRLEN];
    int  done;       // worker sets to 1 when finished
    int  ok;         // 1 on success, 0 on failure
    int  abandoned;  // main set to 1 after timeout; worker frees on exit
    pthread_mutex_t mu;
} DnsJob;

static void *dns_worker(void *arg) {
    DnsJob *j = (DnsJob*)arg;
    struct addrinfo hints;
    struct addrinfo *res = NULL;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_INET;
    int rc = getaddrinfo(j->host, NULL, &hints, &res);
    int ok = 0;
    if (rc == 0 && res != NULL) {
        struct sockaddr_in *rin = (struct sockaddr_in *)res->ai_addr;
        if (inet_ntop(AF_INET, &rin->sin_addr, j->ip, sizeof(j->ip)) != NULL) {
            ok = 1;
        }
    }
    if (res) freeaddrinfo(res);

    pthread_mutex_lock(&j->mu);
    j->ok = ok;
    j->done = 1;
    int abandoned = j->abandoned;
    pthread_mutex_unlock(&j->mu);
    if (abandoned) {
        pthread_mutex_destroy(&j->mu);
        free(j);
    }
    return NULL;
}

const char *spy_socket__gethostbyname_timeout(const char *host_spy, double seconds) {
    char host[256];
    if (spy_socket_str_to_c(host_spy, host, sizeof(host)) < 0) {
        spy_socket_last_err = 2;
        return spy_str_new("", 0);
    }
    if (seconds <= 0.0) {
        // Negative / zero means "no deadline" — fall back to the plain
        // synchronous version.
        return spy_socket__gethostbyname(host_spy);
    }

    DnsJob *job = (DnsJob*)calloc(1, sizeof(DnsJob));
    if (!job) {
        spy_socket_last_err = 2;
        return spy_str_new("", 0);
    }
    memcpy(job->host, host, sizeof(host));
    pthread_mutex_init(&job->mu, NULL);

    pthread_t tid;
    if (pthread_create(&tid, NULL, dns_worker, job) != 0) {
        pthread_mutex_destroy(&job->mu);
        free(job);
        spy_socket_last_err = 2;
        return spy_str_new("", 0);
    }
    pthread_detach(tid);

    struct timespec start;
    clock_gettime(CLOCK_MONOTONIC, &start);
    int timed_out = 0;
    int got_ok = 0;
    char ip_copy[INET_ADDRSTRLEN] = {0};
    while (1) {
        pthread_mutex_lock(&job->mu);
        int done = job->done;
        if (done) {
            got_ok = job->ok;
            if (got_ok) memcpy(ip_copy, job->ip, sizeof(ip_copy));
        }
        pthread_mutex_unlock(&job->mu);
        if (done) break;

        struct timespec now;
        clock_gettime(CLOCK_MONOTONIC, &now);
        double elapsed = (double)(now.tv_sec - start.tv_sec)
                       + (double)(now.tv_nsec - start.tv_nsec) / 1e9;
        if (elapsed >= seconds) {
            timed_out = 1;
            break;
        }
        // 10ms poll interval — coarse enough to be cheap, fine enough
        // that timeouts honour the requested deadline within ~10ms.
        struct timespec slp = {0, 10 * 1000 * 1000};
        nanosleep(&slp, NULL);
    }

    if (timed_out) {
        pthread_mutex_lock(&job->mu);
        job->abandoned = 1;
        int already_done = job->done;
        pthread_mutex_unlock(&job->mu);
        if (already_done) {
            pthread_mutex_destroy(&job->mu);
            free(job);
        }
        // Otherwise the worker frees the job when it eventually returns.
        spy_socket_last_err = 3; // timeout
        return spy_str_new("", 0);
    }

    pthread_mutex_destroy(&job->mu);
    free(job);
    if (!got_ok) {
        spy_socket_last_err = 8;
        return spy_str_new("", 0);
    }
    spy_socket_last_err = 0;
    return spy_str_new(ip_copy, (int64_t)strlen(ip_copy));
}
