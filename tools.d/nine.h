/*
 * nine.h — the Nine sandboxed-tool ABI, for tools written in C.
 *
 * A `kind = "wasm"` tool is a wasm module exporting two functions. This header
 * declares them, provides the packing convention, and wraps the host imports a
 * tool may call, so none of it has to be transcribed from prose.
 *
 * The normative contract is `nine spec toolvm`; the authoring guide is
 * `nine docs writing-sandboxed-tools`. Where this file and those disagree, they
 * are the source of truth and this is a bug.
 *
 * Include it and write one function:
 *
 *     #include "nine.h"
 *
 *     NINE_TOOL(args, len) {
 *         return nine_ok("hello");
 *     }
 *
 * Build (from the repo root, with the SDK `make quickjs-wasm` fetches):
 *
 *     SDK=internal/toolvm/quickjs/.build/wasi-sdk-33
 *     "$SDK/bin/clang" --target=wasm32-wasip1 --sysroot="$SDK/share/wasi-sysroot" \
 *       -mexec-model=reactor -Os -o mytool.wasm mytool.c \
 *       -Wl,--export=nine_alloc -Wl,--export=nine_run -Wl,--strip-all -Wl,--gc-sections
 *
 * There is no `free` anywhere in this ABI, and that is deliberate: the module
 * instance is destroyed when the call returns, so every allocation is reclaimed
 * wholesale and tracking lifetimes buys nothing.
 */

#ifndef NINE_H
#define NINE_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

/* ── ABI version ──────────────────────────────────────────────────────────
 * The guest contract this header speaks. A manifest may state `abi = 1`; it
 * defaults to this and omitting it is the norm.
 */
#define NINE_ABI_VERSION 1

/* ── packing ──────────────────────────────────────────────────────────────
 * nine_run returns one i64: (offset << 32) | length. One value rather than a
 * multi-value return, because every guest language can build an i64.
 *
 * This is the single most common thing to get wrong by hand, which is most of
 * why this header exists.
 */
#define NINE_PACK(ptr, len) \
    (((uint64_t)(uint32_t)(uintptr_t)(ptr) << 32) | (uint32_t)(len))

/* ── the two exports ──────────────────────────────────────────────────────
 *
 *   nine_alloc(size) -> offset
 *     Reserve `size` bytes and return the offset. The host writes the call's
 *     input there before calling nine_run.
 *
 *     The host always asks for one byte MORE than it will write, and writes a
 *     NUL into it. The input is therefore both length-delimited and
 *     NUL-terminated, and may be treated as a C string. That is a guarantee of
 *     the ABI, not an accident of the allocator.
 *
 *   nine_run(ptr, len) -> packed
 *     Run against the `len` bytes of UTF-8 JSON at `ptr`; return the result
 *     packed as above. The result is itself UTF-8 JSON:
 *
 *       {"ok": true,  "output": "..."}
 *       {"ok": false, "error":  "..."}
 *
 *     A false envelope reaches the model as an ordinary tool failure carrying
 *     your message — which is what you want for a bad argument, since the model
 *     can read it and retry.
 */
#define NINE_EXPORT(name) __attribute__((export_name(#name)))

/* The default allocator. Define NINE_NO_ALLOC before including if you want to
 * write your own (you almost certainly do not). */
#ifndef NINE_NO_ALLOC
NINE_EXPORT(nine_alloc) uint32_t nine_alloc(uint32_t size) {
    return (uint32_t)(uintptr_t)malloc(size ? size : 1);
}
#endif

/* Declare the exported nine_run and open its body. */
#define NINE_TOOL(args_name, len_name) \
    NINE_EXPORT(nine_run) uint64_t nine_run(uint32_t args_name##_ptr, uint32_t len_name); \
    NINE_EXPORT(nine_run) uint64_t nine_run(uint32_t args_name##_ptr, uint32_t len_name)

/* Inside a NINE_TOOL body, the input as a NUL-terminated C string. */
#define NINE_ARGS(args_name) ((const char *)(uintptr_t)(args_name##_ptr))

/* ── host imports ─────────────────────────────────────────────────────────
 * The one module a guest may import. `log` is granted to every tool; `http`
 * requires the net.http capability, declared in your manifest and granted by
 * the operator in nine.toml. The import exists either way — a wasm module's
 * imports are fixed at compile time — and permission is checked per call on the
 * host side, where a tool cannot reach it. An ungranted call returns a refusal
 * envelope, not a connection.
 */
__attribute__((import_module("nine"), import_name("log"))) extern void
nine_host_log(const uint8_t *ptr, int32_t len);

__attribute__((import_module("nine"), import_name("http"))) extern uint64_t
nine_host_http(const uint8_t *ptr, int32_t len);

/* Write one line to the daemon log. The only way out of the sandbox that needs
 * no grant. It is not a channel back to the model: the model sees your return
 * value, and nothing else. */
static inline void nine_log(const char *msg) {
    nine_host_log((const uint8_t *)msg, (int32_t)strlen(msg));
}

/*
 * Perform an HTTP request. `request_json` is:
 *
 *   {"url":"https://…","method":"GET","headers":{},"body":""}
 *
 * The response is JSON the host allocated in your memory:
 *
 *   {"status":200,"headers":{…},"body":"…"}   or   {"error":"blocked: …"}
 *
 * `*out_len` receives its length; the returned pointer is NUL-terminated. NULL
 * means the host could not allocate a response at all.
 *
 * Everything policy-shaped — the method and host allowlists, SSRF rejection on
 * the resolved address, per-redirect revalidation, the response cap — is
 * enforced on the host side of this call. A refusal arrives as {"error":…}
 * rather than as a status code, because a refusal is not a response.
 *
 * Note the body is a JSON string: a response that is not valid UTF-8 does not
 * survive it intact. Binary responses are a known gap (`nine docs
 * rich-js-tools`), not something to work around here.
 */
static inline const char *nine_http(const char *request_json, uint32_t *out_len) {
    uint64_t packed = nine_host_http((const uint8_t *)request_json,
                                     (int32_t)strlen(request_json));
    if (packed == 0) {
        if (out_len) *out_len = 0;
        return NULL;
    }
    if (out_len) *out_len = (uint32_t)(packed & 0xffffffff);
    return (const char *)(uintptr_t)(uint32_t)(packed >> 32);
}

/* ── building a result ────────────────────────────────────────────────────
 * Use these rather than sprintf-ing an envelope. An output containing a quote,
 * a backslash, or a newline will corrupt JSON the host is about to parse, and
 * the failure is confusing precisely because it depends on your data. The host
 * shim takes the same care for the same reason.
 */

/* Append `s` to `dst` as the interior of a JSON string, escaping what must be
 * escaped and passing UTF-8 through unchanged. Returns the new length, and
 * never writes more than `cap` bytes. */
static inline size_t nine_json_escape(char *dst, size_t at, size_t cap, const char *s) {
    static const char hex[] = "0123456789abcdef";
    for (; *s && at + 6 < cap; s++) {
        unsigned char c = (unsigned char)*s;
        switch (c) {
        case '"':  dst[at++] = '\\'; dst[at++] = '"';  break;
        case '\\': dst[at++] = '\\'; dst[at++] = '\\'; break;
        case '\n': dst[at++] = '\\'; dst[at++] = 'n';  break;
        case '\r': dst[at++] = '\\'; dst[at++] = 'r';  break;
        case '\t': dst[at++] = '\\'; dst[at++] = 't';  break;
        case '\b': dst[at++] = '\\'; dst[at++] = 'b';  break;
        case '\f': dst[at++] = '\\'; dst[at++] = 'f';  break;
        default:
            if (c < 0x20) {
                dst[at++] = '\\'; dst[at++] = 'u'; dst[at++] = '0'; dst[at++] = '0';
                dst[at++] = hex[c >> 4]; dst[at++] = hex[c & 0xf];
            } else {
                dst[at++] = (char)c; /* UTF-8 passes through */
            }
        }
    }
    return at;
}

/* Internal: build {"ok":<ok>,"<key>":"<text>"} and pack it. */
static inline uint64_t nine__envelope(int ok, const char *key, const char *text) {
    size_t n = strlen(text);
    size_t cap = n * 6 + 64; /* worst case: every byte becomes \u00XX */
    char *buf = (char *)malloc(cap);
    if (!buf) return 0;

    size_t at = 0;
    const char *head = ok ? "{\"ok\":true,\"" : "{\"ok\":false,\"";
    size_t hn = strlen(head);
    memcpy(buf + at, head, hn); at += hn;
    size_t kn = strlen(key);
    memcpy(buf + at, key, kn); at += kn;
    buf[at++] = '"'; buf[at++] = ':'; buf[at++] = '"';
    at = nine_json_escape(buf, at, cap, text);
    buf[at++] = '"'; buf[at++] = '}';
    return NINE_PACK(buf, at);
}

/* Succeed, with `output` as the text the model reads. It is passed through
 * untouched, so it is your own formatting — return what you would want to read
 * in a transcript. */
static inline uint64_t nine_ok(const char *output) {
    return nine__envelope(1, "output", output);
}

/* Fail, with `message` as the error the model reads. Prefer a message that says
 * what to do differently ("date is not ISO-8601") over one that says what went
 * wrong internally. */
static inline uint64_t nine_fail(const char *message) {
    return nine__envelope(0, "error", message);
}

/* Return an envelope you built yourself. `bytes` must be valid UTF-8 JSON of
 * the shape above and must outlive the call — a static buffer or something from
 * nine_alloc/malloc, never a local. */
static inline uint64_t nine_raw(const void *bytes, size_t len) {
    return NINE_PACK(bytes, len);
}

/* ── reading arguments ────────────────────────────────────────────────────
 * There is no JSON parser here. A tool needing one should link a real parser;
 * a tool with one or two string arguments is usually better served by reaching
 * in for the key it wants, which is what this does.
 *
 * Copies the string value of "<key>" into `out` (NUL-terminated, at most
 * `cap` bytes including the terminator) and returns its length, or -1 if the
 * key is absent or not a string. Handles the standard escapes; refuses \u
 * rather than half-handling it.
 */
static inline int32_t nine_arg_str(const char *json, uint32_t json_len,
                                   const char *key, char *out, size_t cap) {
    size_t klen = strlen(key);
    for (uint32_t i = 0; i + klen + 2 <= json_len; i++) {
        if (json[i] != '"' || memcmp(json + i + 1, key, klen) != 0 ||
            json[i + 1 + klen] != '"')
            continue;

        uint32_t j = i + (uint32_t)klen + 2;
        while (j < json_len && (json[j] == ' ' || json[j] == ':')) j++;
        if (j >= json_len || json[j] != '"') return -1;
        j++;

        size_t n = 0;
        while (j < json_len && json[j] != '"' && n + 1 < cap) {
            if (json[j] != '\\') { out[n++] = json[j++]; continue; }
            if (++j >= json_len) return -1;
            switch (json[j]) {
            case 'n':  out[n++] = '\n'; break;
            case 't':  out[n++] = '\t'; break;
            case 'r':  out[n++] = '\r'; break;
            case 'b':  out[n++] = '\b'; break;
            case 'f':  out[n++] = '\f'; break;
            case '"':  out[n++] = '"';  break;
            case '\\': out[n++] = '\\'; break;
            case '/':  out[n++] = '/';  break;
            default: return -1; /* \u is refused rather than half-handled */
            }
            j++;
        }
        out[n] = 0;
        return (int32_t)n;
    }
    return -1;
}

#endif /* NINE_H */
