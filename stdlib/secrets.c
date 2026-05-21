// stdlib/secrets.c — cryptographically-strong tokens for stdlib/secrets.spy.
//
// Randomness comes from /dev/urandom (portable across macOS and Linux), with
// a libc rand() fallback only if that device cannot be opened. Token shapes
// match CPython: token_hex -> 2*n hex chars, token_urlsafe -> unpadded
// base64url, token_bytes -> n raw bytes.

#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include "runtime.h"

static void fill_random(unsigned char *buf, size_t n) {
    FILE *f = fopen("/dev/urandom", "rb");
    if (f) {
        size_t got = fread(buf, 1, n, f);
        fclose(f);
        if (got == n) return;
    }
    for (size_t i = 0; i < n; i++) buf[i] = (unsigned char)(rand() & 0xff);
}

char *spy_secrets__token_bytes(int64_t nbytes) {
    if (nbytes < 0) nbytes = 0;
    unsigned char *buf = (unsigned char *)malloc((size_t)(nbytes > 0 ? nbytes : 1));
    fill_random(buf, (size_t)nbytes);
    char *r = spy_str_new((const char *)buf, nbytes);
    free(buf);
    return r;
}

char *spy_secrets__token_hex(int64_t nbytes) {
    if (nbytes < 0) nbytes = 0;
    static const char HEX[] = "0123456789abcdef";
    unsigned char *buf = (unsigned char *)malloc((size_t)(nbytes > 0 ? nbytes : 1));
    fill_random(buf, (size_t)nbytes);
    char *out = (char *)malloc((size_t)(nbytes * 2 + 1));
    for (int64_t i = 0; i < nbytes; i++) {
        out[i * 2] = HEX[buf[i] >> 4];
        out[i * 2 + 1] = HEX[buf[i] & 0x0f];
    }
    char *r = spy_str_new(out, nbytes * 2);
    free(buf);
    free(out);
    return r;
}

char *spy_secrets__token_urlsafe(int64_t nbytes) {
    if (nbytes < 0) nbytes = 0;
    static const char B64URL[] =
        "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    unsigned char *buf = (unsigned char *)malloc((size_t)(nbytes > 0 ? nbytes : 1));
    fill_random(buf, (size_t)nbytes);
    char *out = (char *)malloc((size_t)((nbytes + 2) / 3 * 4 + 1));
    int64_t op = 0;
    int64_t i = 0;
    while (i + 3 <= nbytes) {
        uint32_t v = ((uint32_t)buf[i] << 16) | ((uint32_t)buf[i + 1] << 8) | (uint32_t)buf[i + 2];
        out[op++] = B64URL[(v >> 18) & 0x3f];
        out[op++] = B64URL[(v >> 12) & 0x3f];
        out[op++] = B64URL[(v >> 6) & 0x3f];
        out[op++] = B64URL[v & 0x3f];
        i += 3;
    }
    int64_t rem = nbytes - i;
    if (rem == 1) {
        uint32_t v = (uint32_t)buf[i] << 16;
        out[op++] = B64URL[(v >> 18) & 0x3f];
        out[op++] = B64URL[(v >> 12) & 0x3f];
    } else if (rem == 2) {
        uint32_t v = ((uint32_t)buf[i] << 16) | ((uint32_t)buf[i + 1] << 8);
        out[op++] = B64URL[(v >> 18) & 0x3f];
        out[op++] = B64URL[(v >> 12) & 0x3f];
        out[op++] = B64URL[(v >> 6) & 0x3f];
    }
    char *r = spy_str_new(out, op);
    free(buf);
    free(out);
    return r;
}

// randbelow(n): uniform int in [0, n) via rejection sampling. n <= 0 -> 0.
int64_t spy_secrets_randbelow(int64_t n) {
    if (n <= 0) return 0;
    uint64_t un = (uint64_t)n;
    uint64_t limit = UINT64_MAX - (UINT64_MAX % un);
    for (;;) {
        uint64_t r;
        fill_random((unsigned char *)&r, sizeof(r));
        if (r < limit) return (int64_t)(r % un);
    }
}

// compare_digest(a, b): constant-time equality over the byte payloads.
int64_t spy_secrets_compare_digest(const char *a, const char *b) {
    int64_t la = spy_str_len(a);
    int64_t lb = spy_str_len(b);
    const unsigned char *da = (const unsigned char *)(a + sizeof(int64_t));
    const unsigned char *db = (const unsigned char *)(b + sizeof(int64_t));
    int64_t n = la < lb ? la : lb;
    unsigned char diff = (unsigned char)(la ^ lb);
    for (int64_t i = 0; i < n; i++) {
        diff |= (unsigned char)(da[i] ^ db[i]);
    }
    return (la == lb && diff == 0) ? 1 : 0;
}
