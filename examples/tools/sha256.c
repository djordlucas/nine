/*
 * sha256.c — a raw `wasm` tool: SHA-256 over a string, in C.
 *
 * Build with `make tools-wasm`, or by hand:
 *
 *   SDK=internal/toolvm/quickjs/.build/wasi-sdk-33
 *   "$SDK/bin/clang" --target=wasm32-wasip1 --sysroot="$SDK/share/wasi-sysroot" \
 *     -mexec-model=reactor -Os -o tools.d/sha256.wasm tools.d/sha256.c \
 *     -Wl,--export=nine_alloc -Wl,--export=nine_run -Wl,--strip-all -Wl,--gc-sections
 *
 * It links no interpreter and knows nothing about JavaScript: it satisfies the
 * same ABI a Rust, TinyGo, or Zig tool would (docs/sandboxed-tools.md §4), via
 * the nine.h header beside it. It takes {"text": "..."} and returns the hex
 * digest.
 */

#include "nine.h"

#include <stdint.h>

/* ── SHA-256 (FIPS 180-4) ─────────────────────────────────────────────────── */

static const uint32_t K[64] = {
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
};

#define ROR(x, n) (((x) >> (n)) | ((x) << (32 - (n))))

static void sha256(const uint8_t *msg, uint32_t len, uint8_t out[32]) {
    uint32_t h[8] = {0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
                     0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19};

    /* Pad: 0x80, zeros, then the bit length as a 64-bit big-endian integer. */
    uint32_t total = ((len + 9 + 63) / 64) * 64;
    uint8_t *buf = calloc(total, 1);
    memcpy(buf, msg, len);
    buf[len] = 0x80;
    uint64_t bits = (uint64_t)len * 8;
    for (int i = 0; i < 8; i++) buf[total - 1 - i] = (uint8_t)(bits >> (8 * i));

    for (uint32_t off = 0; off < total; off += 64) {
        uint32_t w[64];
        for (int i = 0; i < 16; i++) {
            const uint8_t *p = buf + off + i * 4;
            w[i] = ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) | ((uint32_t)p[2] << 8) | p[3];
        }
        for (int i = 16; i < 64; i++) {
            uint32_t s0 = ROR(w[i - 15], 7) ^ ROR(w[i - 15], 18) ^ (w[i - 15] >> 3);
            uint32_t s1 = ROR(w[i - 2], 17) ^ ROR(w[i - 2], 19) ^ (w[i - 2] >> 10);
            w[i] = w[i - 16] + s0 + w[i - 7] + s1;
        }
        uint32_t a = h[0], b = h[1], c = h[2], d = h[3];
        uint32_t e = h[4], f = h[5], g = h[6], hh = h[7];
        for (int i = 0; i < 64; i++) {
            uint32_t S1 = ROR(e, 6) ^ ROR(e, 11) ^ ROR(e, 25);
            uint32_t ch = (e & f) ^ (~e & g);
            uint32_t t1 = hh + S1 + ch + K[i] + w[i];
            uint32_t S0 = ROR(a, 2) ^ ROR(a, 13) ^ ROR(a, 22);
            uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
            uint32_t t2 = S0 + maj;
            hh = g; g = f; f = e; e = d + t1;
            d = c; c = b; b = a; a = t1 + t2;
        }
        h[0] += a; h[1] += b; h[2] += c; h[3] += d;
        h[4] += e; h[5] += f; h[6] += g; h[7] += hh;
    }
    free(buf);

    for (int i = 0; i < 8; i++) {
        out[i * 4 + 0] = (uint8_t)(h[i] >> 24);
        out[i * 4 + 1] = (uint8_t)(h[i] >> 16);
        out[i * 4 + 2] = (uint8_t)(h[i] >> 8);
        out[i * 4 + 3] = (uint8_t)h[i];
    }
}

/* ── the tool ─────────────────────────────────────────────────────────────── */

/* Bounded because a tool's whole input is bounded: the host writes the call's
 * arguments into memory this module caps at [tools] memory_mb. */
#define MAX_TEXT 65536

NINE_TOOL(args, len) {
    static char text[MAX_TEXT];
    static uint8_t raw[MAX_TEXT];
    const uint8_t *msg;
    uint32_t msg_len;

    /* Two ways in. `text` is the ordinary one; `text_b64` exists because hashing
     * is one of the few things you genuinely want to do to *bytes*, and bytes
     * cannot travel in a JSON string — so they arrive base64, exactly as an HTTP
     * response body does. nine_b64_decode is in nine.h for this reason. */
    int32_t n = nine_arg_str(NINE_ARGS(args), len, "text", text, sizeof(text));
    if (n >= 0) {
        msg = (const uint8_t *)text;
        msg_len = (uint32_t)n;
    } else {
        static char b64[MAX_TEXT];
        int32_t bn = nine_arg_str(NINE_ARGS(args), len, "text_b64", b64, sizeof(b64));
        if (bn < 0)
            /* A malformed argument is the textbook non-retryable failure: calling
             * again with the same thing cannot work, and saying so is what stops a
             * model from trying. */
            return nine_fail_code("expected a string argument 'text' or 'text_b64' "
                                  "(\\u escapes are not supported)",
                                  "E_ARGS", NINE_RETRY_NO);
        int32_t rn = nine_b64_decode(b64, raw, sizeof(raw));
        if (rn < 0)
            return nine_fail_code("text_b64 is not valid base64", "E_ARGS", NINE_RETRY_NO);
        msg = raw;
        msg_len = (uint32_t)rn;
    }

    uint8_t digest[32];
    sha256(msg, msg_len, digest);

    static char hexout[65];
    static const char hex[] = "0123456789abcdef";
    for (int i = 0; i < 32; i++) {
        hexout[i * 2] = hex[digest[i] >> 4];
        hexout[i * 2 + 1] = hex[digest[i] & 0xf];
    }
    hexout[64] = 0;

    return nine_ok(hexout);
}
