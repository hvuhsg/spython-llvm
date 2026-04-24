// stdlib/struct.c — binary pack/unpack primitives for stdlib/struct.spy.
//
// Packers append bytes to a spython bytearray (via spy_bytearray_append).
// Unpackers read a value at a given offset inside a spy bytes payload.
// The bytes payload layout is [int64_t len][data...] — same as str.
//
// Endianness is explicit in the symbol name, not derived from the host.
// Floats go through type-punning unions; IEEE-754 layout is assumed for the
// wire, which matches every modern platform.

#include <stdint.h>
#include <string.h>
#include "runtime.h"

// Spython bytes/str payload starts after the int64_t length prefix.
#define SPY_BYTES_DATA(s) ((const unsigned char*)((s) + sizeof(int64_t)))

// -------- byte-at-a-time appenders --------

static void append_byte(char *ba, unsigned char b) {
    spy_bytearray_append(ba, (int64_t)b);
}

// -------- packers --------

void spy_struct_pack_u8(char *ba, int64_t v) {
    append_byte(ba, (unsigned char)(v & 0xff));
}

void spy_struct_pack_i8(char *ba, int64_t v) {
    append_byte(ba, (unsigned char)(v & 0xff));
}

void spy_struct_pack_u16_le(char *ba, int64_t v) {
    uint16_t u = (uint16_t)v;
    append_byte(ba, (unsigned char)(u & 0xff));
    append_byte(ba, (unsigned char)((u >> 8) & 0xff));
}

void spy_struct_pack_u16_be(char *ba, int64_t v) {
    uint16_t u = (uint16_t)v;
    append_byte(ba, (unsigned char)((u >> 8) & 0xff));
    append_byte(ba, (unsigned char)(u & 0xff));
}

void spy_struct_pack_i16_le(char *ba, int64_t v) { spy_struct_pack_u16_le(ba, v); }
void spy_struct_pack_i16_be(char *ba, int64_t v) { spy_struct_pack_u16_be(ba, v); }

void spy_struct_pack_u32_le(char *ba, int64_t v) {
    uint32_t u = (uint32_t)v;
    for (int i = 0; i < 4; i++) {
        append_byte(ba, (unsigned char)((u >> (i * 8)) & 0xff));
    }
}

void spy_struct_pack_u32_be(char *ba, int64_t v) {
    uint32_t u = (uint32_t)v;
    for (int i = 3; i >= 0; i--) {
        append_byte(ba, (unsigned char)((u >> (i * 8)) & 0xff));
    }
}

void spy_struct_pack_i32_le(char *ba, int64_t v) { spy_struct_pack_u32_le(ba, v); }
void spy_struct_pack_i32_be(char *ba, int64_t v) { spy_struct_pack_u32_be(ba, v); }

void spy_struct_pack_u64_le(char *ba, int64_t v) {
    uint64_t u = (uint64_t)v;
    for (int i = 0; i < 8; i++) {
        append_byte(ba, (unsigned char)((u >> (i * 8)) & 0xff));
    }
}

void spy_struct_pack_u64_be(char *ba, int64_t v) {
    uint64_t u = (uint64_t)v;
    for (int i = 7; i >= 0; i--) {
        append_byte(ba, (unsigned char)((u >> (i * 8)) & 0xff));
    }
}

void spy_struct_pack_i64_le(char *ba, int64_t v) { spy_struct_pack_u64_le(ba, v); }
void spy_struct_pack_i64_be(char *ba, int64_t v) { spy_struct_pack_u64_be(ba, v); }

void spy_struct_pack_f32_le(char *ba, double v) {
    union { float f; uint32_t u; } x;
    x.f = (float)v;
    spy_struct_pack_u32_le(ba, (int64_t)x.u);
}

void spy_struct_pack_f32_be(char *ba, double v) {
    union { float f; uint32_t u; } x;
    x.f = (float)v;
    spy_struct_pack_u32_be(ba, (int64_t)x.u);
}

void spy_struct_pack_f64_le(char *ba, double v) {
    union { double f; uint64_t u; } x;
    x.f = v;
    spy_struct_pack_u64_le(ba, (int64_t)x.u);
}

void spy_struct_pack_f64_be(char *ba, double v) {
    union { double f; uint64_t u; } x;
    x.f = v;
    spy_struct_pack_u64_be(ba, (int64_t)x.u);
}

// -------- unpackers --------
//
// No bounds checking at the FFI boundary — callers who need validation
// should check spy_bytes length themselves. v1 keeps this C-flat; we can
// promote to raising IndexError once bytes support that idiom.

int64_t spy_struct_unpack_u8(const char *data, int64_t off) {
    const unsigned char *p = SPY_BYTES_DATA(data);
    return (int64_t)p[off];
}

int64_t spy_struct_unpack_i8(const char *data, int64_t off) {
    const unsigned char *p = SPY_BYTES_DATA(data);
    return (int64_t)(int8_t)p[off];
}

int64_t spy_struct_unpack_u16_le(const char *data, int64_t off) {
    const unsigned char *p = SPY_BYTES_DATA(data) + off;
    return (int64_t)((uint16_t)p[0] | ((uint16_t)p[1] << 8));
}

int64_t spy_struct_unpack_u16_be(const char *data, int64_t off) {
    const unsigned char *p = SPY_BYTES_DATA(data) + off;
    return (int64_t)(((uint16_t)p[0] << 8) | (uint16_t)p[1]);
}

int64_t spy_struct_unpack_i16_le(const char *data, int64_t off) {
    return (int64_t)(int16_t)spy_struct_unpack_u16_le(data, off);
}

int64_t spy_struct_unpack_i16_be(const char *data, int64_t off) {
    return (int64_t)(int16_t)spy_struct_unpack_u16_be(data, off);
}

int64_t spy_struct_unpack_u32_le(const char *data, int64_t off) {
    const unsigned char *p = SPY_BYTES_DATA(data) + off;
    uint32_t u = 0;
    for (int i = 0; i < 4; i++) u |= ((uint32_t)p[i]) << (i * 8);
    return (int64_t)u;
}

int64_t spy_struct_unpack_u32_be(const char *data, int64_t off) {
    const unsigned char *p = SPY_BYTES_DATA(data) + off;
    uint32_t u = 0;
    for (int i = 0; i < 4; i++) u = (u << 8) | (uint32_t)p[i];
    return (int64_t)u;
}

int64_t spy_struct_unpack_i32_le(const char *data, int64_t off) {
    return (int64_t)(int32_t)spy_struct_unpack_u32_le(data, off);
}

int64_t spy_struct_unpack_i32_be(const char *data, int64_t off) {
    return (int64_t)(int32_t)spy_struct_unpack_u32_be(data, off);
}

int64_t spy_struct_unpack_u64_le(const char *data, int64_t off) {
    const unsigned char *p = SPY_BYTES_DATA(data) + off;
    uint64_t u = 0;
    for (int i = 0; i < 8; i++) u |= ((uint64_t)p[i]) << (i * 8);
    return (int64_t)u;
}

int64_t spy_struct_unpack_u64_be(const char *data, int64_t off) {
    const unsigned char *p = SPY_BYTES_DATA(data) + off;
    uint64_t u = 0;
    for (int i = 0; i < 8; i++) u = (u << 8) | (uint64_t)p[i];
    return (int64_t)u;
}

int64_t spy_struct_unpack_i64_le(const char *data, int64_t off) {
    return spy_struct_unpack_u64_le(data, off);
}

int64_t spy_struct_unpack_i64_be(const char *data, int64_t off) {
    return spy_struct_unpack_u64_be(data, off);
}

double spy_struct_unpack_f32_le(const char *data, int64_t off) {
    union { float f; uint32_t u; } x;
    x.u = (uint32_t)spy_struct_unpack_u32_le(data, off);
    return (double)x.f;
}

double spy_struct_unpack_f32_be(const char *data, int64_t off) {
    union { float f; uint32_t u; } x;
    x.u = (uint32_t)spy_struct_unpack_u32_be(data, off);
    return (double)x.f;
}

double spy_struct_unpack_f64_le(const char *data, int64_t off) {
    union { double f; uint64_t u; } x;
    x.u = (uint64_t)spy_struct_unpack_u64_le(data, off);
    return x.f;
}

double spy_struct_unpack_f64_be(const char *data, int64_t off) {
    union { double f; uint64_t u; } x;
    x.u = (uint64_t)spy_struct_unpack_u64_be(data, off);
    return x.f;
}
