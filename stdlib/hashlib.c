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

// Shared SHA-256 driver: takes the 8-word IV and the truncation length so
// SHA-224 (different IV, 28-byte output) reuses the same compression code.
static char *sha256_run(const char *s, const uint32_t iv[8], int out_len) {
    int64_t len = spy_str_len(s);
    const unsigned char *data = (const unsigned char*)SPY_STR_DATA(s);

    uint32_t state[8];
    for (int j = 0; j < 8; j++) state[j] = iv[j];

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
    return hex_to_spy_str(out, out_len);
}

char *spy_hashlib_sha256(const char *s) {
    static const uint32_t iv[8] = {
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
        0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19
    };
    return sha256_run(s, iv, 32);
}

// SHA-224 — FIPS 180-4 §6.3. Same compression as SHA-256 with a different
// IV; the digest is the first 28 bytes (7 of 8 state words).
char *spy_hashlib_sha224(const char *s) {
    static const uint32_t iv[8] = {
        0xc1059ed8, 0x367cd507, 0x3070dd17, 0xf70e5939,
        0xffc00b31, 0x68581511, 0x64f98fa7, 0xbefa4fa4
    };
    return sha256_run(s, iv, 28);
}

// ============================================================================
// SHA-512 / SHA-384 — FIPS 180-4. 64-bit words, 1024-bit blocks, 80 rounds.
// ============================================================================

static const uint64_t SHA512_K[80] = {
    0x428a2f98d728ae22ULL, 0x7137449123ef65cdULL, 0xb5c0fbcfec4d3b2fULL, 0xe9b5dba58189dbbcULL,
    0x3956c25bf348b538ULL, 0x59f111f1b605d019ULL, 0x923f82a4af194f9bULL, 0xab1c5ed5da6d8118ULL,
    0xd807aa98a3030242ULL, 0x12835b0145706fbeULL, 0x243185be4ee4b28cULL, 0x550c7dc3d5ffb4e2ULL,
    0x72be5d74f27b896fULL, 0x80deb1fe3b1696b1ULL, 0x9bdc06a725c71235ULL, 0xc19bf174cf692694ULL,
    0xe49b69c19ef14ad2ULL, 0xefbe4786384f25e3ULL, 0x0fc19dc68b8cd5b5ULL, 0x240ca1cc77ac9c65ULL,
    0x2de92c6f592b0275ULL, 0x4a7484aa6ea6e483ULL, 0x5cb0a9dcbd41fbd4ULL, 0x76f988da831153b5ULL,
    0x983e5152ee66dfabULL, 0xa831c66d2db43210ULL, 0xb00327c898fb213fULL, 0xbf597fc7beef0ee4ULL,
    0xc6e00bf33da88fc2ULL, 0xd5a79147930aa725ULL, 0x06ca6351e003826fULL, 0x142929670a0e6e70ULL,
    0x27b70a8546d22ffcULL, 0x2e1b21385c26c926ULL, 0x4d2c6dfc5ac42aedULL, 0x53380d139d95b3dfULL,
    0x650a73548baf63deULL, 0x766a0abb3c77b2a8ULL, 0x81c2c92e47edaee6ULL, 0x92722c851482353bULL,
    0xa2bfe8a14cf10364ULL, 0xa81a664bbc423001ULL, 0xc24b8b70d0f89791ULL, 0xc76c51a30654be30ULL,
    0xd192e819d6ef5218ULL, 0xd69906245565a910ULL, 0xf40e35855771202aULL, 0x106aa07032bbd1b8ULL,
    0x19a4c116b8d2d0c8ULL, 0x1e376c085141ab53ULL, 0x2748774cdf8eeb99ULL, 0x34b0bcb5e19b48a8ULL,
    0x391c0cb3c5c95a63ULL, 0x4ed8aa4ae3418acbULL, 0x5b9cca4f7763e373ULL, 0x682e6ff3d6b2b8a3ULL,
    0x748f82ee5defb2fcULL, 0x78a5636f43172f60ULL, 0x84c87814a1f0ab72ULL, 0x8cc702081a6439ecULL,
    0x90befffa23631e28ULL, 0xa4506cebde82bde9ULL, 0xbef9a3f7b2c67915ULL, 0xc67178f2e372532bULL,
    0xca273eceea26619cULL, 0xd186b8c721c0c207ULL, 0xeada7dd6cde0eb1eULL, 0xf57d4f7fee6ed178ULL,
    0x06f067aa72176fbaULL, 0x0a637dc5a2c898a6ULL, 0x113f9804bef90daeULL, 0x1b710b35131c471bULL,
    0x28db77f523047d84ULL, 0x32caab7b40c72493ULL, 0x3c9ebe0a15c9bebcULL, 0x431d67c49c100d4cULL,
    0x4cc5d4becb3e42b6ULL, 0x597f299cfc657e2aULL, 0x5fcb6fab3ad6faecULL, 0x6c44198c4a475817ULL
};

static uint64_t sha512_rotr(uint64_t x, int c) { return (x >> c) | (x << (64 - c)); }

static void sha512_block(uint64_t state[8], const unsigned char block[128]) {
    uint64_t w[80];
    for (int i = 0; i < 16; i++) {
        w[i] = ((uint64_t)block[i*8]     << 56)
             | ((uint64_t)block[i*8 + 1] << 48)
             | ((uint64_t)block[i*8 + 2] << 40)
             | ((uint64_t)block[i*8 + 3] << 32)
             | ((uint64_t)block[i*8 + 4] << 24)
             | ((uint64_t)block[i*8 + 5] << 16)
             | ((uint64_t)block[i*8 + 6] <<  8)
             |  (uint64_t)block[i*8 + 7];
    }
    for (int i = 16; i < 80; i++) {
        uint64_t s0 = sha512_rotr(w[i-15], 1) ^ sha512_rotr(w[i-15], 8) ^ (w[i-15] >> 7);
        uint64_t s1 = sha512_rotr(w[i-2], 19) ^ sha512_rotr(w[i-2], 61) ^ (w[i-2] >> 6);
        w[i] = w[i-16] + s0 + w[i-7] + s1;
    }
    uint64_t a = state[0], b = state[1], c = state[2], d = state[3];
    uint64_t e = state[4], f = state[5], g = state[6], h = state[7];
    for (int i = 0; i < 80; i++) {
        uint64_t S1 = sha512_rotr(e, 14) ^ sha512_rotr(e, 18) ^ sha512_rotr(e, 41);
        uint64_t ch = (e & f) ^ ((~e) & g);
        uint64_t temp1 = h + S1 + ch + SHA512_K[i] + w[i];
        uint64_t S0 = sha512_rotr(a, 28) ^ sha512_rotr(a, 34) ^ sha512_rotr(a, 39);
        uint64_t maj = (a & b) ^ (a & c) ^ (b & c);
        uint64_t temp2 = S0 + maj;
        h = g; g = f; f = e; e = d + temp1;
        d = c; c = b; b = a; a = temp1 + temp2;
    }
    state[0] += a; state[1] += b; state[2] += c; state[3] += d;
    state[4] += e; state[5] += f; state[6] += g; state[7] += h;
}

// SHA-512 padding takes a 128-bit length suffix; spython strings can never
// reach 2^64 bits so the high 64 bits are always zero.
static char *sha512_run(const char *s, const uint64_t iv[8], int out_len) {
    int64_t len = spy_str_len(s);
    const unsigned char *data = (const unsigned char*)SPY_STR_DATA(s);

    uint64_t state[8];
    for (int j = 0; j < 8; j++) state[j] = iv[j];

    int64_t i = 0;
    for (; i + 128 <= len; i += 128) sha512_block(state, data + i);

    unsigned char tail[256];
    int64_t rem = len - i;
    memcpy(tail, data + i, (size_t)rem);
    tail[rem] = 0x80;
    int pad_end = (rem < 112) ? 112 : 240;
    for (int64_t j = rem + 1; j < pad_end; j++) tail[j] = 0;
    // 128-bit big-endian length: 64 zero bits + 64-bit bit length.
    for (int j = 0; j < 8; j++) tail[pad_end + j] = 0;
    uint64_t bit_len = (uint64_t)len * 8ULL;
    for (int j = 0; j < 8; j++) tail[pad_end + 8 + 7 - j] = (unsigned char)(bit_len >> (j * 8));
    sha512_block(state, tail);
    if (rem >= 112) sha512_block(state, tail + 128);

    unsigned char out[64];
    for (int j = 0; j < 8; j++) {
        out[j*8]     = (unsigned char)(state[j] >> 56);
        out[j*8 + 1] = (unsigned char)(state[j] >> 48);
        out[j*8 + 2] = (unsigned char)(state[j] >> 40);
        out[j*8 + 3] = (unsigned char)(state[j] >> 32);
        out[j*8 + 4] = (unsigned char)(state[j] >> 24);
        out[j*8 + 5] = (unsigned char)(state[j] >> 16);
        out[j*8 + 6] = (unsigned char)(state[j] >>  8);
        out[j*8 + 7] = (unsigned char)(state[j]);
    }
    return hex_to_spy_str(out, out_len);
}

char *spy_hashlib_sha512(const char *s) {
    static const uint64_t iv[8] = {
        0x6a09e667f3bcc908ULL, 0xbb67ae8584caa73bULL,
        0x3c6ef372fe94f82bULL, 0xa54ff53a5f1d36f1ULL,
        0x510e527fade682d1ULL, 0x9b05688c2b3e6c1fULL,
        0x1f83d9abfb41bd6bULL, 0x5be0cd19137e2179ULL
    };
    return sha512_run(s, iv, 64);
}

char *spy_hashlib_sha384(const char *s) {
    static const uint64_t iv[8] = {
        0xcbbb9d5dc1059ed8ULL, 0x629a292a367cd507ULL,
        0x9159015a3070dd17ULL, 0x152fecd8f70e5939ULL,
        0x67332667ffc00b31ULL, 0x8eb44a8768581511ULL,
        0xdb0c2e0d64f98fa7ULL, 0x47b5481dbefa4fa4ULL
    };
    return sha512_run(s, iv, 48);
}
