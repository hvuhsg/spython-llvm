// spython-link: -lm
//
// stdlib/random.c — xoshiro256** PRNG with splitmix64 seed expansion.
// Public-domain algorithms by Blackman & Vigna (prng.di.unimi.it) and
// Steele Jr. et al. No external dependencies beyond libm for the
// continuous distributions.

#include <stdint.h>
#include <time.h>
#include <math.h>

static uint64_t state[4];
static int initialized = 0;

static uint64_t rotl(uint64_t x, int k) {
    return (x << k) | (x >> (64 - k));
}

static uint64_t next_u64(void) {
    const uint64_t result = rotl(state[1] * 5, 7) * 9;
    const uint64_t t = state[1] << 17;
    state[2] ^= state[0];
    state[3] ^= state[1];
    state[1] ^= state[2];
    state[0] ^= state[3];
    state[2] ^= t;
    state[3] = rotl(state[3], 45);
    return result;
}

static uint64_t splitmix64(uint64_t *x) {
    uint64_t z = (*x += 0x9e3779b97f4a7c15ULL);
    z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ULL;
    z = (z ^ (z >> 27)) * 0x94d049bb133111ebULL;
    return z ^ (z >> 31);
}

static void do_seed(uint64_t seed) {
    uint64_t x = seed;
    state[0] = splitmix64(&x);
    state[1] = splitmix64(&x);
    state[2] = splitmix64(&x);
    state[3] = splitmix64(&x);
    // xoshiro256** requires at least one non-zero state word; splitmix64
    // with non-zero increment guarantees this for any seed.
    initialized = 1;
}

static void ensure_init(void) {
    if (!initialized) {
        do_seed((uint64_t)time(NULL));
    }
}

void spy_random_seed(int64_t seed_val) {
    do_seed((uint64_t)seed_val);
}

double spy_random_random(void) {
    ensure_init();
    // 53-bit mantissa fill — the canonical way to map a 64-bit integer to
    // a double in [0.0, 1.0).
    return (double)(next_u64() >> 11) * (1.0 / (double)(1ULL << 53));
}

int64_t spy_random_randint(int64_t a, int64_t b) {
    ensure_init();
    if (b < a) return a;
    uint64_t range = (uint64_t)(b - a) + 1ULL;
    // Modulo introduces a slight bias for very large ranges; fine for
    // stdlib-level use.
    return a + (int64_t)(next_u64() % range);
}

double spy_random_uniform(double a, double b) {
    ensure_init();
    double r = (double)(next_u64() >> 11) * (1.0 / (double)(1ULL << 53));
    return a + (b - a) * r;
}

int64_t spy_random_getrandbits(int64_t n) {
    ensure_init();
    if (n <= 0) return 0;
    if (n >= 64) return (int64_t)next_u64();
    return (int64_t)(next_u64() & ((1ULL << n) - 1ULL));
}

// Internal: a fresh double in [0.0, 1.0).
static double rnd(void) {
    ensure_init();
    return (double)(next_u64() >> 11) * (1.0 / (double)(1ULL << 53));
}

// normalvariate: Kinderman-Monahan ratio-of-uniforms (CPython's algorithm).
double spy_random_normalvariate(double mu, double sigma) {
    const double NV_MAGICCONST = 1.7155277699214135; // 4*exp(-0.5)/sqrt(2)
    double z;
    for (;;) {
        double u1 = rnd();
        double u2 = 1.0 - rnd();
        z = NV_MAGICCONST * (u1 - 0.5) / u2;
        if (z * z / 4.0 <= -log(u2)) break;
    }
    return mu + z * sigma;
}

// gauss: Box-Muller. Faster than normalvariate; same N(mu, sigma) result.
double spy_random_gauss(double mu, double sigma) {
    double u1 = 1.0 - rnd();
    double u2 = rnd();
    double z = sqrt(-2.0 * log(u1)) * cos(2.0 * M_PI * u2);
    return mu + z * sigma;
}

double spy_random_lognormvariate(double mu, double sigma) {
    return exp(spy_random_normalvariate(mu, sigma));
}

double spy_random_expovariate(double lambd) {
    return -log(1.0 - rnd()) / lambd;
}

double spy_random_paretovariate(double alpha) {
    double u = 1.0 - rnd();
    return pow(u, -1.0 / alpha);
}

double spy_random_weibullvariate(double alpha, double beta) {
    double u = 1.0 - rnd();
    return alpha * pow(-log(u), 1.0 / beta);
}

double spy_random_triangular(double low, double high, double mode) {
    double u = rnd();
    if (high == low) return low;
    double c = (mode - low) / (high - low);
    if (u > c) {
        u = 1.0 - u;
        c = 1.0 - c;
        double t = low; low = high; high = t;
    }
    return low + (high - low) * sqrt(u * c);
}
