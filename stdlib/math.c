// spython-link: -lm
//
// stdlib/math.c — thin wrappers around <math.h>, matching the names declared
// in math.spy's @extern decorators (default mangling: spy_math_<name>).

#include <math.h>
#include <stdint.h>

double spy_math_sqrt(double x) { return sqrt(x); }
double spy_math_sin(double x)  { return sin(x); }
double spy_math_cos(double x)  { return cos(x); }
double spy_math_tan(double x)  { return tan(x); }
double spy_math_asin(double x) { return asin(x); }
double spy_math_acos(double x) { return acos(x); }
double spy_math_atan(double x) { return atan(x); }
double spy_math_atan2(double y, double x) { return atan2(y, x); }

double spy_math_sinh(double x) { return sinh(x); }
double spy_math_cosh(double x) { return cosh(x); }
double spy_math_tanh(double x) { return tanh(x); }
double spy_math_asinh(double x) { return asinh(x); }
double spy_math_acosh(double x) { return acosh(x); }
double spy_math_atanh(double x) { return atanh(x); }

double spy_math_log(double x)   { return log(x); }
double spy_math_log2(double x)  { return log2(x); }
double spy_math_log10(double x) { return log10(x); }
double spy_math_log1p(double x) { return log1p(x); }
double spy_math_exp(double x)   { return exp(x); }
double spy_math_expm1(double x) { return expm1(x); }
double spy_math_pow(double base, double exponent) { return pow(base, exponent); }

double spy_math_floor(double x) { return floor(x); }
double spy_math_ceil(double x)  { return ceil(x); }
double spy_math_trunc(double x) { return trunc(x); }
double spy_math_fabs(double x)  { return fabs(x); }
double spy_math_fmod(double x, double y) { return fmod(x, y); }
double spy_math_copysign(double x, double y) { return copysign(x, y); }
// CPython's math.ldexp takes (x, i) where i is a Python int; we receive it
// as int64 and cast — overflow on 32-bit `int` exponents is libc-defined.
double spy_math_ldexp(double x, int64_t i) { return ldexp(x, (int)i); }

double spy_math_degrees(double x) { return x * (180.0 / 3.141592653589793); }
double spy_math_radians(double x) { return x * (3.141592653589793 / 180.0); }

double spy_math_erf(double x)    { return erf(x); }
double spy_math_erfc(double x)   { return erfc(x); }
double spy_math_gamma(double x)  { return tgamma(x); }
double spy_math_lgamma(double x) { return lgamma(x); }

// Predicates return 1/0 — spython's bool ABI is i1 (0 or 1).
int64_t spy_math_isfinite(double x) { return isfinite(x) ? 1 : 0; }
int64_t spy_math_isinf(double x)    { return isinf(x) ? 1 : 0; }
int64_t spy_math_isnan(double x)    { return isnan(x) ? 1 : 0; }

// factorial: matches CPython for n >= 0. Negative n returns 0; CPython
// raises ValueError, but spython's @extern surface has no exception path,
// so we degrade to 0 (callers can guard with `if n < 0`).
int64_t spy_math_factorial(int64_t n) {
    if (n < 0) return 0;
    int64_t r = 1;
    for (int64_t i = 2; i <= n; i++) r *= i;
    return r;
}

// isqrt: floor(sqrt(n)) using Newton's method on integers, matching CPython.
int64_t spy_math_isqrt(int64_t n) {
    if (n < 0) return 0;  // CPython raises ValueError; see factorial above.
    if (n < 2) return n;
    int64_t x = n;
    int64_t y = (x + 1) / 2;
    while (y < x) {
        x = y;
        y = (x + n / x) / 2;
    }
    return x;
}

double spy_math__inf(void) { return INFINITY; }
double spy_math__nan(void) { return NAN; }

// comb(n, k) and perm(n, k): exact int64 results (overflow is the caller's
// concern, as with factorial). k < 0 in perm means k = n.
int64_t spy_math_comb(int64_t n, int64_t k) {
    if (n < 0 || k < 0 || k > n) return 0;
    if (k > n - k) k = n - k;
    int64_t r = 1;
    for (int64_t i = 0; i < k; i++) {
        r = r * (n - i) / (i + 1);
    }
    return r;
}

int64_t spy_math__perm(int64_t n, int64_t k) {
    if (k < 0) k = n;
    if (n < 0 || k > n) return 0;
    int64_t r = 1;
    for (int64_t i = 0; i < k; i++) {
        r *= (n - i);
    }
    return r;
}

// frexp / modf split a double into two parts; spython returns them as a
// tuple built in the .spy wrapper from these two scalar reads.
double spy_math__frexp_m(double x) { int e; return frexp(x, &e); }
int64_t spy_math__frexp_e(double x) { int e; frexp(x, &e); return (int64_t)e; }
double spy_math__modf_frac(double x) { double ip; return modf(x, &ip); }
double spy_math__modf_int(double x) { double ip; modf(x, &ip); return ip; }

double spy_math_remainder(double x, double y) { return remainder(x, y); }
double spy_math_nextafter(double x, double y) { return nextafter(x, y); }

// ulp(x): spacing to the next representable double away from zero.
double spy_math_ulp(double x) {
    if (isnan(x)) return x;
    x = fabs(x);
    if (isinf(x)) return x;
    double up = nextafter(x, INFINITY);
    if (isinf(up)) return x - nextafter(x, -INFINITY);
    return up - x;
}
