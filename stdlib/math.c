// spython-link: -lm
//
// stdlib/math.c — thin wrappers around <math.h>, matching the names declared
// in math.spy's @extern decorators (default mangling: spy_math_<name>).

#include <math.h>

double spy_math_sqrt(double x) { return sqrt(x); }
double spy_math_sin(double x)  { return sin(x); }
double spy_math_cos(double x)  { return cos(x); }
double spy_math_tan(double x)  { return tan(x); }
double spy_math_asin(double x) { return asin(x); }
double spy_math_acos(double x) { return acos(x); }
double spy_math_atan(double x) { return atan(x); }
double spy_math_atan2(double y, double x) { return atan2(y, x); }
double spy_math_log(double x)   { return log(x); }
double spy_math_log2(double x)  { return log2(x); }
double spy_math_log10(double x) { return log10(x); }
double spy_math_exp(double x)   { return exp(x); }
double spy_math_pow(double base, double exponent) { return pow(base, exponent); }
double spy_math_floor(double x) { return floor(x); }
double spy_math_ceil(double x)  { return ceil(x); }
double spy_math_fabs(double x)  { return fabs(x); }
double spy_math_fmod(double x, double y) { return fmod(x, y); }

double spy_math__inf(void) { return INFINITY; }
double spy_math__nan(void) { return NAN; }
