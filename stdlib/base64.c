// stdlib/base64.c — RFC 4648 Base64 codec for stdlib/base64.spy.
//
// Operates on spython str/bytes payloads: [int64_t len][data...]. No
// whitespace tolerance on decode; callers should strip newlines upstream
// if they have MIME-formatted input.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "runtime.h"

#define SPY_DATA(s) ((const unsigned char*)((s) + sizeof(int64_t)))

static const char B64_ALPHABET[] =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

// 0-63 for valid base64 characters, 64 for '=', -1 for everything else.
// Built lazily on first decode.
static int8_t b64_decode_table[256];
static int b64_decode_table_ready = 0;

static void b64_build_decode_table(void) {
    for (int i = 0; i < 256; i++) b64_decode_table[i] = -1;
    for (int i = 0; i < 64; i++) b64_decode_table[(unsigned char)B64_ALPHABET[i]] = (int8_t)i;
    b64_decode_table['='] = 64;
    b64_decode_table_ready = 1;
}

char *spy_base64_b64encode(const char *src) {
    int64_t in_len = spy_str_len(src);
    const unsigned char *in = SPY_DATA(src);
    int64_t out_len = ((in_len + 2) / 3) * 4;
    char *buf = (char*)malloc((size_t)(out_len > 0 ? out_len : 1));
    int64_t i = 0, j = 0;
    while (i + 2 < in_len) {
        uint32_t v = ((uint32_t)in[i] << 16) | ((uint32_t)in[i+1] << 8) | (uint32_t)in[i+2];
        buf[j++] = B64_ALPHABET[(v >> 18) & 0x3f];
        buf[j++] = B64_ALPHABET[(v >> 12) & 0x3f];
        buf[j++] = B64_ALPHABET[(v >> 6) & 0x3f];
        buf[j++] = B64_ALPHABET[v & 0x3f];
        i += 3;
    }
    int64_t rem = in_len - i;
    if (rem == 1) {
        uint32_t v = (uint32_t)in[i] << 16;
        buf[j++] = B64_ALPHABET[(v >> 18) & 0x3f];
        buf[j++] = B64_ALPHABET[(v >> 12) & 0x3f];
        buf[j++] = '=';
        buf[j++] = '=';
    } else if (rem == 2) {
        uint32_t v = ((uint32_t)in[i] << 16) | ((uint32_t)in[i+1] << 8);
        buf[j++] = B64_ALPHABET[(v >> 18) & 0x3f];
        buf[j++] = B64_ALPHABET[(v >> 12) & 0x3f];
        buf[j++] = B64_ALPHABET[(v >> 6) & 0x3f];
        buf[j++] = '=';
    }
    char *result = spy_str_new(buf, out_len);
    free(buf);
    return result;
}

char *spy_base64_b64decode(const char *src) {
    if (!b64_decode_table_ready) b64_build_decode_table();
    int64_t in_len = spy_str_len(src);
    const unsigned char *in = SPY_DATA(src);
    // Length must be a multiple of 4. Reject otherwise by returning empty.
    if (in_len % 4 != 0) return spy_str_new("", 0);
    int64_t max_out = (in_len / 4) * 3;
    char *buf = (char*)malloc((size_t)(max_out > 0 ? max_out : 1));
    int64_t j = 0;
    for (int64_t i = 0; i < in_len; i += 4) {
        int8_t a = b64_decode_table[in[i]];
        int8_t b = b64_decode_table[in[i+1]];
        int8_t c = b64_decode_table[in[i+2]];
        int8_t d = b64_decode_table[in[i+3]];
        if (a < 0 || b < 0 || c < 0 || d < 0 || a == 64 || b == 64) {
            free(buf);
            return spy_str_new("", 0);
        }
        uint32_t v = ((uint32_t)a << 18) | ((uint32_t)b << 12);
        if (c == 64) {
            // "XX==" — only one real byte.
            if (d != 64 || i + 4 != in_len) { free(buf); return spy_str_new("", 0); }
            buf[j++] = (char)((v >> 16) & 0xff);
        } else if (d == 64) {
            // "XXX=" — two real bytes.
            if (i + 4 != in_len) { free(buf); return spy_str_new("", 0); }
            v |= ((uint32_t)c << 6);
            buf[j++] = (char)((v >> 16) & 0xff);
            buf[j++] = (char)((v >> 8) & 0xff);
        } else {
            v |= ((uint32_t)c << 6) | (uint32_t)d;
            buf[j++] = (char)((v >> 16) & 0xff);
            buf[j++] = (char)((v >> 8) & 0xff);
            buf[j++] = (char)(v & 0xff);
        }
    }
    char *result = spy_str_new(buf, j);
    free(buf);
    return result;
}

// Base16 (hex) per RFC 4648: uppercase on encode, case-insensitive on
// decode. Invalid input to decode yields empty bytes (matching the lenient
// b64decode behaviour above).
char *spy_base64__b16encode(const char *src) {
    static const char HEX[] = "0123456789ABCDEF";
    int64_t in_len = spy_str_len(src);
    const unsigned char *in = SPY_DATA(src);
    char *buf = (char *)malloc((size_t)(in_len * 2 + 1));
    for (int64_t i = 0; i < in_len; i++) {
        buf[i * 2] = HEX[in[i] >> 4];
        buf[i * 2 + 1] = HEX[in[i] & 0x0f];
    }
    char *result = spy_str_new(buf, in_len * 2);
    free(buf);
    return result;
}

static int hexval(unsigned char c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

char *spy_base64__b16decode(const char *src) {
    int64_t in_len = spy_str_len(src);
    const unsigned char *in = SPY_DATA(src);
    if (in_len % 2 != 0) return spy_str_new("", 0);
    char *buf = (char *)malloc((size_t)(in_len / 2 > 0 ? in_len / 2 : 1));
    int64_t j = 0;
    for (int64_t i = 0; i + 1 < in_len; i += 2) {
        int hi = hexval(in[i]);
        int lo = hexval(in[i + 1]);
        if (hi < 0 || lo < 0) { free(buf); return spy_str_new("", 0); }
        buf[j++] = (char)((hi << 4) | lo);
    }
    char *result = spy_str_new(buf, j);
    free(buf);
    return result;
}
