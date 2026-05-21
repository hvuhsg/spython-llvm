// spython-link: -lssl -lcrypto
//
// stdlib/ssl.c — TLS client wrapping for stdlib/ssl.spy. Backed by
// OpenSSL (or LibreSSL — the surface here is the OpenSSL 1.1+/3.x
// public API, which both implement). Linked via `-lssl -lcrypto`.
//
// Scope: TLS *client* sockets only. The caller hands us a connected
// blocking AF_INET / SOCK_STREAM fd produced by socket.connect(), plus
// a server hostname for SNI and certificate verification. We perform
// the TLS handshake, then expose SSL_read / SSL_write through send /
// recv. Server-side TLS, custom CA bundles, client certificates, and
// session resumption are out of scope for v1.
//
// Handle model: SSL_CTX and SSL pointers cannot cross the spython FFI
// boundary as opaque pointers (the only available scalar types are
// int64 and the heap-managed strings/bytes), so we keep two small
// static slot tables and hand back integer indices. Slots fill from
// index 1 upwards; index 0 means "unused / NULL". Closing a slot
// writes NULL back into it.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

#include <openssl/ssl.h>
#include <openssl/err.h>
#include <openssl/x509v3.h>

#include "runtime.h"

#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

#define MAX_CTX 64
#define MAX_SSL 1024

static SSL_CTX *ctx_table[MAX_CTX];
static int      ctx_inited = 0;

static SSL     *ssl_table[MAX_SSL];

static char    last_error[512] = {0};

static void ssl_set_err(const char *prefix) {
    unsigned long e = ERR_get_error();
    if (e == 0) {
        snprintf(last_error, sizeof(last_error), "%s", prefix);
        return;
    }
    char buf[256];
    ERR_error_string_n(e, buf, sizeof(buf));
    snprintf(last_error, sizeof(last_error), "%s: %s", prefix, buf);
}

static void ssl_init_once(void) {
    if (ctx_inited) return;
    // OpenSSL 1.1.0+ auto-initialises on first use; calling these is a
    // no-op there but harmless. We avoid the legacy SSL_library_init /
    // SSL_load_error_strings calls (deprecated in 1.1, removed in 3.x).
    OPENSSL_init_ssl(OPENSSL_INIT_LOAD_SSL_STRINGS
                     | OPENSSL_INIT_LOAD_CRYPTO_STRINGS, NULL);
    ctx_inited = 1;
}

// Allocate a fresh slot in `table` (length cap) and return its 1-based
// index, or -1 if the table is full.
static int alloc_slot_ctx(SSL_CTX *p) {
    for (int i = 1; i < MAX_CTX; i++) {
        if (ctx_table[i] == NULL) { ctx_table[i] = p; return i; }
    }
    return -1;
}

static int alloc_slot_ssl(SSL *p) {
    for (int i = 1; i < MAX_SSL; i++) {
        if (ssl_table[i] == NULL) { ssl_table[i] = p; return i; }
    }
    return -1;
}

// Public ABI ----------------------------------------------------------

// Allocate a TLS client SSL_CTX with default CA verification enabled
// against the system trust store (SSL_CTX_set_default_verify_paths).
// Returns a positive handle on success, -1 on failure (last_error set).
int64_t spy_ssl__ctx_new(void) {
    ssl_init_once();
    SSL_CTX *ctx = SSL_CTX_new(TLS_client_method());
    if (!ctx) {
        ssl_set_err("SSL_CTX_new");
        return -1;
    }
    // Require TLS 1.2 minimum — older versions are insecure and most
    // public servers have removed them.
    SSL_CTX_set_min_proto_version(ctx, TLS1_2_VERSION);
    SSL_CTX_set_verify(ctx, SSL_VERIFY_PEER, NULL);
    if (SSL_CTX_set_default_verify_paths(ctx) != 1) {
        ssl_set_err("SSL_CTX_set_default_verify_paths");
        SSL_CTX_free(ctx);
        return -1;
    }
    int slot = alloc_slot_ctx(ctx);
    if (slot < 0) {
        SSL_CTX_free(ctx);
        snprintf(last_error, sizeof(last_error), "ssl: too many contexts");
        return -1;
    }
    return (int64_t)slot;
}

// Disable peer verification on the given context. Useful for testing
// against self-signed servers; production callers should leave the
// default. Returns 0 on success, -1 if the handle is invalid.
int64_t spy_ssl__ctx_set_verify_none(int64_t ctx_h) {
    if (ctx_h <= 0 || ctx_h >= MAX_CTX || ctx_table[ctx_h] == NULL) return -1;
    SSL_CTX_set_verify(ctx_table[ctx_h], SSL_VERIFY_NONE, NULL);
    return 0;
}

int64_t spy_ssl__ctx_free(int64_t ctx_h) {
    if (ctx_h <= 0 || ctx_h >= MAX_CTX || ctx_table[ctx_h] == NULL) return -1;
    SSL_CTX_free(ctx_table[ctx_h]);
    ctx_table[ctx_h] = NULL;
    return 0;
}

// Wrap an already-connected blocking TCP fd in TLS, perform the
// handshake, and return a positive SSL handle. server_hostname is the
// SNI value AND the name used for certificate verification. Returns -1
// on failure with last_error populated.
int64_t spy_ssl__wrap(int64_t ctx_h, int64_t fd, const char *server_hostname) {
    if (ctx_h <= 0 || ctx_h >= MAX_CTX || ctx_table[ctx_h] == NULL) {
        snprintf(last_error, sizeof(last_error), "ssl: invalid context");
        return -1;
    }
    SSL *ssl = SSL_new(ctx_table[ctx_h]);
    if (!ssl) { ssl_set_err("SSL_new"); return -1; }
    if (SSL_set_fd(ssl, (int)fd) != 1) {
        ssl_set_err("SSL_set_fd");
        SSL_free(ssl);
        return -1;
    }
    int64_t host_len = spy_str_len(server_hostname);
    if (host_len > 0) {
        const char *host = SPY_STR_DATA(server_hostname);
        // Copy to a NUL-terminated buffer for the OpenSSL APIs.
        char *cstr = (char*)malloc((size_t)host_len + 1);
        memcpy(cstr, host, (size_t)host_len);
        cstr[host_len] = 0;
        // SNI extension.
        SSL_set_tlsext_host_name(ssl, cstr);
        // Hostname check on the leaf cert.
        X509_VERIFY_PARAM *param = SSL_get0_param(ssl);
        X509_VERIFY_PARAM_set_hostflags(param, X509_CHECK_FLAG_NO_PARTIAL_WILDCARDS);
        X509_VERIFY_PARAM_set1_host(param, cstr, (size_t)host_len);
        free(cstr);
    }
    SSL_set_connect_state(ssl);
    int rc = SSL_connect(ssl);
    if (rc != 1) {
        ssl_set_err("SSL_connect");
        SSL_free(ssl);
        return -1;
    }
    int slot = alloc_slot_ssl(ssl);
    if (slot < 0) {
        SSL_shutdown(ssl);
        SSL_free(ssl);
        snprintf(last_error, sizeof(last_error), "ssl: too many sockets");
        return -1;
    }
    return (int64_t)slot;
}

// Send a single buffer; returns the number of bytes accepted by SSL_write
// or -1 on error. Partial writes are reported as the byte count; the
// spython side loops until the buffer is drained.
int64_t spy_ssl__send(int64_t ssl_h, const char *data) {
    if (ssl_h <= 0 || ssl_h >= MAX_SSL || ssl_table[ssl_h] == NULL) {
        snprintf(last_error, sizeof(last_error), "ssl: invalid handle");
        return -1;
    }
    int64_t n = spy_str_len(data);
    if (n == 0) return 0;
    int rc = SSL_write(ssl_table[ssl_h], SPY_STR_DATA(data), (int)n);
    if (rc <= 0) { ssl_set_err("SSL_write"); return -1; }
    return (int64_t)rc;
}

// Receive at most `n` bytes. Returns the bytes actually read as a fresh
// spython bytes value; an empty result means EOF (the peer closed).
char* spy_ssl__recv(int64_t ssl_h, int64_t n) {
    if (ssl_h <= 0 || ssl_h >= MAX_SSL || ssl_table[ssl_h] == NULL) {
        snprintf(last_error, sizeof(last_error), "ssl: invalid handle");
        return spy_str_new("", 0);
    }
    if (n <= 0) return spy_str_new("", 0);
    char *buf = (char*)malloc((size_t)n);
    int rc = SSL_read(ssl_table[ssl_h], buf, (int)n);
    if (rc < 0) {
        ssl_set_err("SSL_read");
        free(buf);
        return spy_str_new("", 0);
    }
    if (rc == 0) {
        // Clean shutdown — return empty.
        free(buf);
        return spy_str_new("", 0);
    }
    char *out = spy_str_new(buf, rc);
    free(buf);
    return out;
}

int64_t spy_ssl__close(int64_t ssl_h) {
    if (ssl_h <= 0 || ssl_h >= MAX_SSL || ssl_table[ssl_h] == NULL) return -1;
    SSL *ssl = ssl_table[ssl_h];
    // Best-effort bidirectional shutdown. Single SSL_shutdown sends our
    // close_notify; we don't bother waiting for the peer's.
    SSL_shutdown(ssl);
    SSL_free(ssl);
    ssl_table[ssl_h] = NULL;
    return 0;
}

// Return the most recent error string (a fresh spython str). Empty
// when there hasn't been a failure since startup.
char* spy_ssl__last_error(void) {
    return spy_str_new(last_error, (int64_t)strlen(last_error));
}
