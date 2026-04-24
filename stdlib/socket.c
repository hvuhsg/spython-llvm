// stdlib/socket.c — POSIX-backed TCP socket primitives for stdlib/socket.spy.
// Scope: IPv4 (AF_INET) + TCP (SOCK_STREAM) clients. Blocking sockets only.
//
// The str/bytes ABI is the same shared layout as io.c: a char* pointing at
// [int64_t len][data...]. send() reads that layout directly; recv() allocates
// a fresh buffer via spy_str_new so the caller receives a spython bytes value.

#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include <errno.h>
#include <unistd.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <gc.h>
#include "runtime.h"

// Last-error classification, populated by every primitive that can fail.
// Matches the order decoded by socket.spy's _last_error_class wrapper:
//   0 = ok, 1 = ECONNREFUSED, 2 = other.
static int spy_socket_last_err = 0;

static void spy_socket_record_errno(void) {
    switch (errno) {
        case 0:            spy_socket_last_err = 0; return;
        case ECONNREFUSED: spy_socket_last_err = 1; return;
        default:           spy_socket_last_err = 2; return;
    }
}

int64_t spy_socket__last_error_class(void) {
    return (int64_t)spy_socket_last_err;
}

int64_t spy_socket__socket(int64_t family, int64_t type) {
    int fd = socket((int)family, (int)type, 0);
    if (fd < 0) {
        spy_socket_record_errno();
        return -1;
    }
    spy_socket_last_err = 0;
    return (int64_t)fd;
}

int64_t spy_socket__connect(int64_t fd, const char *host_spy, int64_t port) {
    int64_t host_len = *(int64_t*)host_spy;
    if (host_len < 0 || host_len > 253) {
        spy_socket_last_err = 2;
        return -1;
    }
    char host[256];
    memcpy(host, host_spy + sizeof(int64_t), (size_t)host_len);
    host[host_len] = '\0';

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons((uint16_t)port);

    // Try numeric IPv4 first; fall back to getaddrinfo for DNS.
    if (inet_pton(AF_INET, host, &addr.sin_addr) != 1) {
        struct addrinfo hints;
        struct addrinfo *res = NULL;
        memset(&hints, 0, sizeof(hints));
        hints.ai_family = AF_INET;
        hints.ai_socktype = SOCK_STREAM;
        if (getaddrinfo(host, NULL, &hints, &res) != 0 || res == NULL) {
            spy_socket_last_err = 2;
            if (res) freeaddrinfo(res);
            return -1;
        }
        struct sockaddr_in *rin = (struct sockaddr_in *)res->ai_addr;
        addr.sin_addr = rin->sin_addr;
        freeaddrinfo(res);
    }

    int rc = connect((int)fd, (struct sockaddr *)&addr, sizeof(addr));
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
            spy_socket_record_errno();
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
    char *tmp = (char *)malloc((size_t)(n > 0 ? n : 1));
    ssize_t got = 0;
    for (;;) {
        got = recv((int)fd, tmp, (size_t)n, 0);
        if (got < 0 && errno == EINTR) continue;
        break;
    }
    if (got < 0) {
        spy_socket_record_errno();
        got = 0;
    } else {
        spy_socket_last_err = 0;
    }
    char *out = spy_str_new(tmp, (int64_t)got);
    free(tmp);
    return out;
}

void spy_socket__close(int64_t fd) {
    if (fd < 0) return;
    close((int)fd);
}
