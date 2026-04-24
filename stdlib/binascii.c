// stdlib/binascii.c — hex codec and CRC-32 (IEEE 802.3, same polynomial as
// zlib/Python's binascii.crc32). Uses spython's string ABI from runtime.h.

#include <stdint.h>
#include <stdlib.h>
#include "runtime.h"

// Spython string layout: [int64_t len][data...]. The payload starts after the
// length prefix.
#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

static const char HEX_DIGITS[] = "0123456789abcdef";

char *spy_binascii_hexlify(const char *s) {
    int64_t len = spy_str_len(s);
    const char *data = SPY_STR_DATA(s);
    int64_t out_len = len * 2;
    char *buf = (char*)malloc((size_t)out_len > 0 ? (size_t)out_len : 1);
    for (int64_t i = 0; i < len; i++) {
        unsigned char c = (unsigned char)data[i];
        buf[i * 2]     = HEX_DIGITS[c >> 4];
        buf[i * 2 + 1] = HEX_DIGITS[c & 0x0f];
    }
    char *result = spy_str_new(buf, out_len);
    free(buf);
    return result;
}

static int hex_val(char c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

char *spy_binascii_unhexlify(const char *s) {
    int64_t len = spy_str_len(s);
    const char *data = SPY_STR_DATA(s);
    if (len % 2 != 0) {
        // Odd length is malformed; return empty string. Exceptions are not
        // yet supported across the FFI boundary.
        return spy_str_new("", 0);
    }
    int64_t out_len = len / 2;
    char *buf = (char*)malloc((size_t)out_len > 0 ? (size_t)out_len : 1);
    for (int64_t i = 0; i < out_len; i++) {
        int hi = hex_val(data[i * 2]);
        int lo = hex_val(data[i * 2 + 1]);
        if (hi < 0 || lo < 0) {
            free(buf);
            return spy_str_new("", 0);
        }
        buf[i] = (char)((hi << 4) | lo);
    }
    char *result = spy_str_new(buf, out_len);
    free(buf);
    return result;
}

// CRC-32 table for polynomial 0xEDB88320 (reversed 0x04C11DB7). Computed
// lazily on first use.
static uint32_t crc32_table[256];
static int crc32_table_ready = 0;

static void crc32_build_table(void) {
    for (uint32_t i = 0; i < 256; i++) {
        uint32_t c = i;
        for (int k = 0; k < 8; k++) {
            c = (c & 1) ? (0xEDB88320u ^ (c >> 1)) : (c >> 1);
        }
        crc32_table[i] = c;
    }
    crc32_table_ready = 1;
}

int64_t spy_binascii_crc32(const char *s) {
    if (!crc32_table_ready) crc32_build_table();
    int64_t len = spy_str_len(s);
    const unsigned char *data = (const unsigned char*)SPY_STR_DATA(s);
    uint32_t crc = 0xFFFFFFFFu;
    for (int64_t i = 0; i < len; i++) {
        crc = crc32_table[(crc ^ data[i]) & 0xff] ^ (crc >> 8);
    }
    return (int64_t)(crc ^ 0xFFFFFFFFu);
}
