// stdlib/re.c — POSIX-regex backend for stdlib/re.spy. Uses libc's
// regcomp/regexec (POSIX ERE), so the recognised pattern syntax is
// strictly POSIX extended regular expressions, not Python re. The .spy
// shim documents the syntax delta. No extra link flags are required —
// regex.h is part of libc on Darwin / BSD / Linux.
//
// State model. Each call to spy_re__exec performs a fresh regexec into
// a static result table; the spython layer reads the per-group offsets
// out via the _group_count / _group_start / _group_end accessors and
// builds a Match object before issuing the next call. This keeps the C
// surface tiny (no opaque handles) at the cost of forcing the spython
// side to copy offsets eagerly, which it does in Match.__init__.
//
// Compile cache. Module-level helpers like findall / finditer / sub run
// regexec in a loop with the same pattern; recompiling on every step
// would dominate runtime. We keep a single-slot cache (pattern bytes +
// flags) and reuse the compiled regex_t when the next call matches.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <regex.h>
#include "runtime.h"

#define SPY_STR_DATA(s) ((const char*)((s) + sizeof(int64_t)))

// Mode for spy_re__exec.
//   0 SEARCH    — leftmost match anywhere at or after `start`.
//   1 MATCH     — match must begin exactly at `start`.
//   2 FULLMATCH — match must begin at `start` and consume to end of input.
#define MODE_SEARCH    0
#define MODE_MATCH     1
#define MODE_FULLMATCH 2

// spython-side flag bits (mirror the constants in re.spy).
#define SPY_RE_IGNORECASE 1
#define SPY_RE_MULTILINE  2

// Capture-group capacity. POSIX exposes re->re_nsub; the +1 covers
// group 0 (the full match). 64 capture groups is more than any realistic
// regex.
#define MAX_GROUPS 64

// Last-call result table.
static int     last_group_count = 0;
static int64_t last_starts[MAX_GROUPS];
static int64_t last_ends[MAX_GROUPS];

// Last compile error message, materialised on demand by _error_msg.
static char last_error_buf[256] = {0};

// Single-slot compile cache.
static char  *cached_pat_data = NULL;   // malloc'd null-terminated copy
static int64_t cached_pat_len = -1;
static int    cached_flags    = -1;
static int    cached_valid    = 0;
static regex_t cached_re;

static void clear_match_state(void) {
    last_group_count = 0;
}

static int map_cflags(int64_t spy_flags) {
    int cf = REG_EXTENDED;
    if (spy_flags & SPY_RE_IGNORECASE) cf |= REG_ICASE;
    if (spy_flags & SPY_RE_MULTILINE)  cf |= REG_NEWLINE;
    return cf;
}

// Compile `pat` (length-prefixed spython string) under `spy_flags`. Returns
// a pointer to the cached regex_t on success. On failure populates
// last_error_buf and returns NULL.
static regex_t *get_compiled(const char *pat_spy, int64_t spy_flags) {
    int64_t pat_len = spy_str_len(pat_spy);
    const char *pat_data = SPY_STR_DATA(pat_spy);
    int cf = map_cflags(spy_flags);

    if (cached_valid
        && cached_flags == cf
        && cached_pat_len == pat_len
        && memcmp(cached_pat_data, pat_data, (size_t)pat_len) == 0) {
        return &cached_re;
    }

    if (cached_valid) {
        regfree(&cached_re);
        free(cached_pat_data);
        cached_pat_data = NULL;
        cached_valid = 0;
    }

    char *patz = (char*)malloc((size_t)pat_len + 1);
    memcpy(patz, pat_data, (size_t)pat_len);
    patz[pat_len] = '\0';

    int rc = regcomp(&cached_re, patz, cf);
    if (rc != 0) {
        regerror(rc, &cached_re, last_error_buf, sizeof(last_error_buf));
        // POSIX requires regfree even after a failed regcomp.
        regfree(&cached_re);
        free(patz);
        return NULL;
    }

    cached_pat_data = patz;
    cached_pat_len = pat_len;
    cached_flags = cf;
    cached_valid = 1;
    return &cached_re;
}

// Run a compiled regex against s[start:] under the given mode. Returns
//   1 if a match was found and recorded in last_*,
//   0 if no match,
//  -1 if pattern compilation failed (read message via _error_msg).
int64_t spy_re__exec(const char *pat_spy, const char *s_spy,
                     int64_t start, int64_t flags, int64_t mode) {
    int64_t s_len = spy_str_len(s_spy);
    if (start < 0) start = 0;
    if (start > s_len) {
        clear_match_state();
        return 0;
    }

    regex_t *re = get_compiled(pat_spy, flags);
    if (!re) {
        clear_match_state();
        return -1;
    }

    // POSIX regexec wants a null-terminated C string. Copy the suffix and
    // append a nul. spython strings can technically contain embedded \0,
    // but POSIX regex treats any \0 as end-of-string — that's a documented
    // limitation of this engine, not something we can fix here.
    int64_t sub_len = s_len - start;
    char *buf = (char*)malloc((size_t)sub_len + 1);
    memcpy(buf, SPY_STR_DATA(s_spy) + start, (size_t)sub_len);
    buf[sub_len] = '\0';

    regmatch_t pmatch[MAX_GROUPS];
    int eflags = 0;
    if (start > 0) eflags |= REG_NOTBOL;

    int rc = regexec(re, buf, MAX_GROUPS, pmatch, eflags);
    free(buf);

    if (rc == REG_NOMATCH) {
        clear_match_state();
        return 0;
    }
    if (rc != 0) {
        regerror(rc, re, last_error_buf, sizeof(last_error_buf));
        clear_match_state();
        return -1;
    }

    // Mode anchoring. POSIX regexec returns the leftmost match in the
    // (sub)string we passed in; for MATCH and FULLMATCH we additionally
    // require that the match begin at position 0 (== `start` in s) and,
    // for FULLMATCH, that it consume the entire suffix.
    if (mode == MODE_MATCH && pmatch[0].rm_so != 0) {
        clear_match_state();
        return 0;
    }
    if (mode == MODE_FULLMATCH
        && (pmatch[0].rm_so != 0 || pmatch[0].rm_eo != (regoff_t)sub_len)) {
        clear_match_state();
        return 0;
    }

    int n = (int)re->re_nsub + 1;
    if (n > MAX_GROUPS) n = MAX_GROUPS;
    last_group_count = n;
    for (int i = 0; i < n; i++) {
        if (pmatch[i].rm_so < 0) {
            last_starts[i] = -1;
            last_ends[i]   = -1;
        } else {
            last_starts[i] = (int64_t)pmatch[i].rm_so + start;
            last_ends[i]   = (int64_t)pmatch[i].rm_eo + start;
        }
    }
    return 1;
}

int64_t spy_re__group_count(void) {
    return (int64_t)last_group_count;
}

// Group offsets. Returns -1 if `i` is out of range or if the group did
// not participate in the match. The spython side treats -1 as the sentinel
// for "no value".
int64_t spy_re__group_start(int64_t i) {
    if (i < 0 || i >= last_group_count) return -1;
    return last_starts[i];
}

int64_t spy_re__group_end(int64_t i) {
    if (i < 0 || i >= last_group_count) return -1;
    return last_ends[i];
}

char *spy_re__error_msg(void) {
    return spy_str_new(last_error_buf, (int64_t)strlen(last_error_buf));
}
