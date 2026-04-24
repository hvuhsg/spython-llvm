// stdlib/hashlib.c — compact self-contained MD5, SHA-1, SHA-256.
// All three are implemented from their respective specifications
// (RFC 1321 for MD5, FIPS 180 for SHA-1, FIPS 180-2 for SHA-256).
// No external dependencies; no link flags required.

#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include "runtime.h"

#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

static const char HEX[] = "0123456789abcdef";

static char *hex_to_spy_str(const unsigned char *bytes, int n) {
    char *buf = (char*)malloc((size_t)(n * 2));
    for (int i = 0; i < n; i++) {
        buf[i * 2]     = HEX[bytes[i] >> 4];
        buf[i * 2 + 1] = HEX[bytes[i] & 0x0f];
    }
    char *result = spy_str_new(buf, (int64_t)(n * 2));
    free(buf);
    return result;
}

// ============================================================================
// MD5 — RFC 1321
// ============================================================================

static const uint32_t MD5_K[64] = {
    0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee, 0xf57c0faf, 0x4787c62a, 0xa8304613, 0xfd469501,
    0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be, 0x6b901122, 0xfd987193, 0xa679438e, 0x49b40821,
    0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa, 0xd62f105d, 0x02441453, 0xd8a1e681, 0xe7d3fbc8,
    0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed, 0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a,
    0xfffa3942, 0x8771f681, 0x6d9d6122, 0xfde5380c, 0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70,
    0x289b7ec6, 0xeaa127fa, 0xd4ef3085, 0x04881d05, 0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665,
    0xf4292244, 0x432aff97, 0xab9423a7, 0xfc93a039, 0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
    0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1, 0xf7537e82, 0xbd3af235, 0x2ad7d2bb, 0xeb86d391
};

static const int MD5_R[64] = {
    7,12,17,22, 7,12,17,22, 7,12,17,22, 7,12,17,22,
    5, 9,14,20, 5, 9,14,20, 5, 9,14,20, 5, 9,14,20,
    4,11,16,23, 4,11,16,23, 4,11,16,23, 4,11,16,23,
    6,10,15,21, 6,10,15,21, 6,10,15,21, 6,10,15,21
};

static uint32_t md5_rotl(uint32_t x, int c) { return (x << c) | (x >> (32 - c)); }

static void md5_block(uint32_t state[4], const unsigned char block[64]) {
    uint32_t m[16];
    for (int i = 0; i < 16; i++) {
        m[i] = (uint32_t)block[i*4]
             | ((uint32_t)block[i*4+1] << 8)
             | ((uint32_t)block[i*4+2] << 16)
             | ((uint32_t)block[i*4+3] << 24);
    }
    uint32_t a = state[0], b = state[1], c = state[2], d = state[3];
    for (int i = 0; i < 64; i++) {
        uint32_t f;
        int g;
        if (i < 16)      { f = (b & c) | ((~b) & d);      g = i; }
        else if (i < 32) { f = (d & b) | ((~d) & c);      g = (5 * i + 1) % 16; }
        else if (i < 48) { f = b ^ c ^ d;                 g = (3 * i + 5) % 16; }
        else             { f = c ^ (b | (~d));            g = (7 * i) % 16; }
        uint32_t temp = d;
        d = c;
        c = b;
        b = b + md5_rotl(a + f + MD5_K[i] + m[g], MD5_R[i]);
        a = temp;
    }
    state[0] += a; state[1] += b; state[2] += c; state[3] += d;
}

char *spy_hashlib_md5(const char *s) {
    int64_t len = spy_str_len(s);
    const unsigned char *data = (const unsigned char*)SPY_STR_DATA(s);

    uint32_t state[4] = {0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476};

    // Process full 64-byte blocks.
    int64_t i = 0;
    for (; i + 64 <= len; i += 64) md5_block(state, data + i);

    // Final block(s): tail + 0x80 + zero padding + 64-bit length (little-endian).
    unsigned char tail[128];
    int64_t rem = len - i;
    memcpy(tail, data + i, (size_t)rem);
    tail[rem] = 0x80;
    int pad_end = (rem < 56) ? 56 : 120;
    for (int64_t j = rem + 1; j < pad_end; j++) tail[j] = 0;
    uint64_t bit_len = (uint64_t)len * 8ULL;
    for (int j = 0; j < 8; j++) tail[pad_end + j] = (unsigned char)(bit_len >> (j * 8));
    md5_block(state, tail);
    if (rem >= 56) md5_block(state, tail + 64);

    unsigned char out[16];
    for (int j = 0; j < 4; j++) {
        out[j*4]     = (unsigned char)(state[j]);
        out[j*4 + 1] = (unsigned char)(state[j] >> 8);
        out[j*4 + 2] = (unsigned char)(state[j] >> 16);
        out[j*4 + 3] = (unsigned char)(state[j] >> 24);
    }
    return hex_to_spy_str(out, 16);
}

// ============================================================================
// SHA-1 — FIPS 180
// ============================================================================

static uint32_t sha1_rotl(uint32_t x, int c) { return (x << c) | (x >> (32 - c)); }

static void sha1_block(uint32_t state[5], const unsigned char block[64]) {
    uint32_t w[80];
    for (int i = 0; i < 16; i++) {
        w[i] = ((uint32_t)block[i*4]     << 24)
             | ((uint32_t)block[i*4 + 1] << 16)
             | ((uint32_t)block[i*4 + 2] <<  8)
             |  (uint32_t)block[i*4 + 3];
    }
    for (int i = 16; i < 80; i++) {
        w[i] = sha1_rotl(w[i-3] ^ w[i-8] ^ w[i-14] ^ w[i-16], 1);
    }
    uint32_t a = state[0], b = state[1], c = state[2], d = state[3], e = state[4];
    for (int i = 0; i < 80; i++) {
        uint32_t f, k;
        if (i < 20)      { f = (b & c) | ((~b) & d);   k = 0x5A827999; }
        else if (i < 40) { f = b ^ c ^ d;              k = 0x6ED9EBA1; }
        else if (i < 60) { f = (b & c) | (b & d) | (c & d); k = 0x8F1BBCDC; }
        else             { f = b ^ c ^ d;              k = 0xCA62C1D6; }
        uint32_t temp = sha1_rotl(a, 5) + f + e + k + w[i];
        e = d; d = c; c = sha1_rotl(b, 30); b = a; a = temp;
    }
    state[0] += a; state[1] += b; state[2] += c; state[3] += d; state[4] += e;
}

char *spy_hashlib_sha1(const char *s) {
    int64_t len = spy_str_len(s);
    const unsigned char *data = (const unsigned char*)SPY_STR_DATA(s);

    uint32_t state[5] = {0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0};

    int64_t i = 0;
    for (; i + 64 <= len; i += 64) sha1_block(state, data + i);

    unsigned char tail[128];
    int64_t rem = len - i;
    memcpy(tail, data + i, (size_t)rem);
    tail[rem] = 0x80;
    int pad_end = (rem < 56) ? 56 : 120;
    for (int64_t j = rem + 1; j < pad_end; j++) tail[j] = 0;
    uint64_t bit_len = (uint64_t)len * 8ULL;
    // Big-endian length.
    for (int j = 0; j < 8; j++) tail[pad_end + 7 - j] = (unsigned char)(bit_len >> (j * 8));
    sha1_block(state, tail);
    if (rem >= 56) sha1_block(state, tail + 64);

    unsigned char out[20];
    for (int j = 0; j < 5; j++) {
        out[j*4]     = (unsigned char)(state[j] >> 24);
        out[j*4 + 1] = (unsigned char)(state[j] >> 16);
        out[j*4 + 2] = (unsigned char)(state[j] >>  8);
        out[j*4 + 3] = (unsigned char)(state[j]);
    }
    return hex_to_spy_str(out, 20);
}

// ============================================================================
// SHA-256 — FIPS 180-2
// ============================================================================

static const uint32_t SHA256_K[64] = {
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
};

static uint32_t sha256_rotr(uint32_t x, int c) { return (x >> c) | (x << (32 - c)); }

static void sha256_block(uint32_t state[8], const unsigned char block[64]) {
    uint32_t w[64];
    for (int i = 0; i < 16; i++) {
        w[i] = ((uint32_t)block[i*4]     << 24)
             | ((uint32_t)block[i*4 + 1] << 16)
             | ((uint32_t)block[i*4 + 2] <<  8)
             |  (uint32_t)block[i*4 + 3];
    }
    for (int i = 16; i < 64; i++) {
        uint32_t s0 = sha256_rotr(w[i-15], 7) ^ sha256_rotr(w[i-15], 18) ^ (w[i-15] >> 3);
        uint32_t s1 = sha256_rotr(w[i-2], 17) ^ sha256_rotr(w[i-2], 19) ^ (w[i-2] >> 10);
        w[i] = w[i-16] + s0 + w[i-7] + s1;
    }
    uint32_t a = state[0], b = state[1], c = state[2], d = state[3];
    uint32_t e = state[4], f = state[5], g = state[6], h = state[7];
    for (int i = 0; i < 64; i++) {
        uint32_t S1 = sha256_rotr(e, 6) ^ sha256_rotr(e, 11) ^ sha256_rotr(e, 25);
        uint32_t ch = (e & f) ^ ((~e) & g);
        uint32_t temp1 = h + S1 + ch + SHA256_K[i] + w[i];
        uint32_t S0 = sha256_rotr(a, 2) ^ sha256_rotr(a, 13) ^ sha256_rotr(a, 22);
        uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
        uint32_t temp2 = S0 + maj;
        h = g; g = f; f = e; e = d + temp1;
        d = c; c = b; b = a; a = temp1 + temp2;
    }
    state[0] += a; state[1] += b; state[2] += c; state[3] += d;
    state[4] += e; state[5] += f; state[6] += g; state[7] += h;
}

char *spy_hashlib_sha256(const char *s) {
    int64_t len = spy_str_len(s);
    const unsigned char *data = (const unsigned char*)SPY_STR_DATA(s);

    uint32_t state[8] = {
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
        0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19
    };

    int64_t i = 0;
    for (; i + 64 <= len; i += 64) sha256_block(state, data + i);

    unsigned char tail[128];
    int64_t rem = len - i;
    memcpy(tail, data + i, (size_t)rem);
    tail[rem] = 0x80;
    int pad_end = (rem < 56) ? 56 : 120;
    for (int64_t j = rem + 1; j < pad_end; j++) tail[j] = 0;
    uint64_t bit_len = (uint64_t)len * 8ULL;
    for (int j = 0; j < 8; j++) tail[pad_end + 7 - j] = (unsigned char)(bit_len >> (j * 8));
    sha256_block(state, tail);
    if (rem >= 56) sha256_block(state, tail + 64);

    unsigned char out[32];
    for (int j = 0; j < 8; j++) {
        out[j*4]     = (unsigned char)(state[j] >> 24);
        out[j*4 + 1] = (unsigned char)(state[j] >> 16);
        out[j*4 + 2] = (unsigned char)(state[j] >>  8);
        out[j*4 + 3] = (unsigned char)(state[j]);
    }
    return hex_to_spy_str(out, 32);
}
