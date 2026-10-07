/*
 * qjs_host.c — the Nine ABI over a bare QuickJS-NG core.
 *
 * This is the whole of the `js` tool kind's native surface. It exports the two
 * functions of the Nine wasm ABI (see internal/toolvm/abi.go) and imports one
 * host function, `nine.log`. It links neither quickjs-libc nor any part of it,
 * so `std` and `os` — and with them a filesystem API, os.exec, std.urlGet, and
 * std.evalScript — do not exist in this interpreter at all
 * (docs/sandboxed-tools.md §4.1). They are not denied; they were never built.
 *
 * The other job here is §4.3: module resolution happens in the host, against a
 * closed allowlist, before instantiation. The guest never receives a resolver
 * that can touch disk or network — see nine_module_normalize/nine_module_load,
 * which serve from the `modules` object of the call envelope and refuse
 * everything else.
 */

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <errno.h>

#include "quickjs.h"

/* ── the host imports ─────────────────────────────────────────────────────
 * The `log` capability (§6.2). Granted by default because it leaks nothing:
 * the daemon decides where the bytes go (slog + the event journal).
 */
__attribute__((import_module("nine"), import_name("log"))) extern void
nine_host_log(const uint8_t *ptr, int32_t len);

/* The `net.http` capability (§8). The import exists unconditionally — a wasm
 * module's imports are fixed at compile time and this blob is shared by every
 * `js` tool — but the *grant* is checked host-side, per call. A tool without one
 * gets a refusal envelope back, not a connection. The guest never touches a
 * socket and never learns an IP.
 *
 * Returns (offset << 32) | length of a JSON response the host allocated through
 * nine_alloc, or 0 if it could not allocate one. */
__attribute__((import_module("nine"), import_name("http"))) extern uint64_t
nine_host_http(const uint8_t *ptr, int32_t len);

/* The `state` capability: a host-owned key/value store scoped to this tool, so a
 * tool can remember across calls without anything surviving the instance. Like
 * http the import exists unconditionally — imports are fixed at compile time and
 * this blob is shared by every `js` tool — and the grant is checked host-side per
 * call, so a tool without one gets a refusal envelope rather than a store.
 *
 * Takes a JSON op ({"op":"get","key":"…"}) and returns (offset << 32) | length of
 * a JSON response the host allocated through nine_alloc, or 0. */
__attribute__((import_module("nine"), import_name("state"))) extern uint64_t
nine_host_state(const uint8_t *ptr, int32_t len);

/* The `process` capability: what a live process uses to drive its session —
 * wait for its next trigger, run a model turn, report to a pipe. Like state it
 * takes a JSON op ({"op":"next"}) and returns (offset << 32) | length of a JSON
 * response, or 0. The import exists unconditionally; the host refuses every op
 * unless this instance was started as a live process, so an ordinary call — a
 * tool the model invoked — can never block in it. */
__attribute__((import_module("nine"), import_name("process"))) extern uint64_t
nine_host_process(const uint8_t *ptr, int32_t len);

/* The calling tool's resolved capability grant, as JSON. It confers nothing —
 * every capability is enforced elsewhere, by wazero's pre-opens or by the host's
 * per-call grant lookup — and exists so a guest can say "fs.read is not granted
 * to this tool" instead of surfacing an ENOENT for a file that plainly exists.
 *
 * Returns (offset << 32) | length of a JSON document the host allocated through
 * nine_alloc, or 0. */
__attribute__((import_module("nine"), import_name("caps"))) extern uint64_t
nine_host_caps(void);

/* ── ABI: allocation ──────────────────────────────────────────────────────
 * The host calls nine_alloc, writes the envelope into the returned offset,
 * then calls nine_run. There is no nine_free: the instance is destroyed when
 * the call returns (§3), so every allocation is reclaimed wholesale.
 */
__attribute__((export_name("nine_alloc"))) uint32_t nine_alloc(uint32_t size) {
    return (uint32_t)(uintptr_t)malloc(size ? size : 1);
}

/* Pack a (pointer, length) pair into the single i64 the ABI returns. Multi-value
 * returns would work in current toolchains, but every guest language can build
 * an i64, and that keeps a raw-.wasm tool author's job trivial. */
static inline uint64_t pack(const void *ptr, size_t len) {
    return ((uint64_t)(uint32_t)(uintptr_t)ptr << 32) | (uint32_t)len;
}

/* Copy a NUL-terminated string into a fresh buffer and return it packed. Used
 * for every exit path, so the host reads results uniformly. */
static uint64_t pack_owned(const char *s) {
    size_t n = strlen(s);
    char *buf = malloc(n ? n : 1);
    if (!buf) return pack(NULL, 0);
    memcpy(buf, s, n);
    return pack(buf, n);
}

/* ── module resolution ───────────────────────────────────────────────────── */

/* The set of modules this call may import, taken from the envelope's `modules`
 * object. The host builds it from its allowlist; anything absent is unavailable
 * regardless of what the source asks for. */
typedef struct {
    JSValue modules;
} nine_modules;

/* Refuse anything that is not a verbatim key of the modules object.
 *
 * The default QuickJS normalizer resolves a specifier against the importing
 * module's name as if it were a path, which is precisely the behavior §4.3
 * forbids: it makes "./x", "../x", and "/etc/x" meaningful. This one performs
 * no resolution at all — a specifier either names an allowed module exactly or
 * it is an error — so relative imports, absolute paths, and URLs all fail the
 * same way, and dynamic import() inherits the same refusal.
 */
static char *nine_module_normalize(JSContext *ctx, const char *base_name,
                                   const char *name, void *opaque) {
    nine_modules *set = opaque;
    (void)base_name;

    JSValue src = JS_GetPropertyStr(ctx, set->modules, name);
    int ok = JS_IsString(src);
    JS_FreeValue(ctx, src);
    if (!ok) {
        JS_ThrowReferenceError(ctx, "module \"%s\" is not available to this tool", name);
        return NULL;
    }
    return js_strdup(ctx, name);
}

/* ── ABI: the harness ─────────────────────────────────────────────────────
 *
 * The host writes the precompiled harness — QuickJS module bytecode, built from
 * harness.js by `make harness-bc` — into guest memory and names it here, once,
 * before nine_run.
 *
 * It is bytecode rather than source because parsing *was* the call. Of a 6 ms
 * call, 4.8 ms was the host escaping 32 KB of harness into JSON, the guest
 * parsing that JSON, and QuickJS compiling the result — every call, to produce
 * the same program every time. Executing the harness costs 0.14 ms by
 * comparison. JS_ReadObject skips all of the first and none of the second.
 *
 * It rides beside the envelope rather than inside it because the envelope is
 * JSON and this is bytes: base64 would have cost more than the source did.
 *
 * The pointer is into this instance's own linear memory and the instance is
 * destroyed when the call returns (§3), so these statics cannot outlive the
 * call that set them or be read by another.
 */
static const uint8_t *g_harness = NULL;
static uint32_t g_harness_len = 0;

__attribute__((export_name("nine_harness"))) void nine_harness(uint32_t ptr,
                                                               uint32_t len) {
    g_harness = (const uint8_t *)(uintptr_t)ptr;
    g_harness_len = len;
}

static JSModuleDef *nine_module_load(JSContext *ctx, const char *name, void *opaque) {
    nine_modules *set = opaque;

    JSValue src = JS_GetPropertyStr(ctx, set->modules, name);
    if (!JS_IsString(src)) {
        JS_FreeValue(ctx, src);
        JS_ThrowReferenceError(ctx, "module \"%s\" is not available to this tool", name);
        return NULL;
    }

    size_t len = 0;
    const char *code = JS_ToCStringLen(ctx, &len, src);
    JS_FreeValue(ctx, src);
    if (!code) return NULL;

    JSValue mod = JS_Eval(ctx, code, len, name,
                          JS_EVAL_TYPE_MODULE | JS_EVAL_FLAG_COMPILE_ONLY);
    JS_FreeCString(ctx, code);
    if (JS_IsException(mod)) return NULL;

    JSModuleDef *def = JS_VALUE_GET_PTR(mod);
    JS_FreeValue(ctx, mod);
    return def;
}

/* ── the log host function, as the guest sees it ─────────────────────────── */

static JSValue js_nine_log(JSContext *ctx, JSValueConst this_val, int argc,
                           JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_UNDEFINED;

    size_t len = 0;
    const char *msg = JS_ToCStringLen(ctx, &len, argv[0]);
    if (!msg) return JS_EXCEPTION;
    nine_host_log((const uint8_t *)msg, (int32_t)len);
    JS_FreeCString(ctx, msg);
    return JS_UNDEFINED;
}

/* fetch, as the guest sees it: JSON request string in, JSON response string
 * out. Everything policy-shaped — method allowlist, host allowlist, SSRF
 * rejection, redirect revalidation, response caps — happens on the host side of
 * this call, where it can be reasoned about. */
static JSValue js_nine_http(JSContext *ctx, JSValueConst this_val, int argc,
                            JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "http requires a request object");

    size_t len = 0;
    const char *req = JS_ToCStringLen(ctx, &len, argv[0]);
    if (!req) return JS_EXCEPTION;

    uint64_t packed = nine_host_http((const uint8_t *)req, (int32_t)len);
    JS_FreeCString(ctx, req);

    if (packed == 0) return JS_ThrowInternalError(ctx, "http: no response from host");

    const char *out = (const char *)(uintptr_t)(uint32_t)(packed >> 32);
    uint32_t out_len = (uint32_t)(packed & 0xffffffff);
    JSValue res = JS_NewStringLen(ctx, out, out_len);
    free((void *)out);
    return res;
}

static JSValue js_nine_state(JSContext *ctx, JSValueConst this_val, int argc,
                             JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "state requires a request object");

    size_t len = 0;
    const char *req = JS_ToCStringLen(ctx, &len, argv[0]);
    if (!req) return JS_EXCEPTION;

    uint64_t packed = nine_host_state((const uint8_t *)req, (int32_t)len);
    JS_FreeCString(ctx, req);

    if (packed == 0) return JS_ThrowInternalError(ctx, "state: no response from host");

    const char *out = (const char *)(uintptr_t)(uint32_t)(packed >> 32);
    uint32_t out_len = (uint32_t)(packed & 0xffffffff);
    JSValue res = JS_NewStringLen(ctx, out, out_len);
    free((void *)out);
    return res;
}

static JSValue js_nine_process(JSContext *ctx, JSValueConst this_val, int argc,
                               JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "process requires a request object");

    size_t len = 0;
    const char *req = JS_ToCStringLen(ctx, &len, argv[0]);
    if (!req) return JS_EXCEPTION;

    uint64_t packed = nine_host_process((const uint8_t *)req, (int32_t)len);
    JS_FreeCString(ctx, req);

    if (packed == 0) return JS_ThrowInternalError(ctx, "process: no response from host");

    const char *out = (const char *)(uintptr_t)(uint32_t)(packed >> 32);
    uint32_t out_len = (uint32_t)(packed & 0xffffffff);
    JSValue res = JS_NewStringLen(ctx, out, out_len);
    free((void *)out);
    return res;
}

/* ── filesystem, environment, randomness ──────────────────────────────────
 *
 * These exist because a `js` tool could not reach three capabilities an operator
 * can grant it. `fs` and `env` are WASI facilities — wazero pre-opens and
 * WithEnv — which a raw .wasm tool reaches through libc, while this interpreter
 * deliberately links no quickjs-libc and therefore had no binding for either
 * (adr/rich-js-tools.md §1). The grant was real, the enforcement was real, and
 * nothing in JavaScript could use it.
 *
 * They are ordinary libc calls, NOT new host functions, and that is the whole
 * design. wasi-libc is already linked here; fopen and getenv route through WASI
 * to exactly the pre-opens and env pairs the host configured. So containment
 * stays wazero's — a tool scoped to /srv/data cannot walk out of it without our
 * writing a single check — instead of becoming a path-checking function of ours
 * that we would then own the bugs in (§6.4).
 *
 * Nothing here linked std or os; a tool with no grant sees an empty filesystem
 * and an empty environment, because that is what the host handed the instance.
 */

#include <stdio.h>
#include <dirent.h>
#include <sys/stat.h>
#include <unistd.h>

/* Read a whole file. Returns a Uint8Array — bytes rather than text, so that a
 * tool reading a PNG does not get the U+FFFD treatment binary HTTP bodies used
 * to get. Decoding is the caller's choice, via TextDecoder. */
static JSValue js_nine_fs_read(JSContext *ctx, JSValueConst this_val, int argc,
                               JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "readFile requires a path");
    const char *path = JS_ToCString(ctx, argv[0]);
    if (!path) return JS_EXCEPTION;

    FILE *f = fopen(path, "rb");
    if (!f) {
        JSValue e = JS_ThrowTypeError(ctx, "cannot read %s", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    size_t cap = 65536, len = 0;
    uint8_t *buf = malloc(cap);
    if (!buf) { fclose(f); JS_FreeCString(ctx, path); return JS_ThrowOutOfMemory(ctx); }
    for (;;) {
        if (len == cap) {
            size_t ncap = cap * 2;
            uint8_t *nb = realloc(buf, ncap);
            if (!nb) { free(buf); fclose(f); JS_FreeCString(ctx, path); return JS_ThrowOutOfMemory(ctx); }
            buf = nb; cap = ncap;
        }
        size_t n = fread(buf + len, 1, cap - len, f);
        if (n == 0) {
            /* Distinguish end-of-file from an I/O error: without this, a failed
             * read returns a silently truncated file as success, which is the
             * exact failure mode this whole surface exists to avoid. */
            if (ferror(f)) {
                free(buf);
                fclose(f);
                JSValue e = JS_ThrowTypeError(ctx, "error reading %s", path);
                JS_FreeCString(ctx, path);
                return e;
            }
            break;
        }
        len += n;
    }
    fclose(f);
    JS_FreeCString(ctx, path);

    JSValue out = JS_NewUint8ArrayCopy(ctx, buf, len);
    free(buf);
    return out;
}

/* Write a file. Accepts a string (written as UTF-8) or any ArrayBuffer/view. */
static JSValue js_nine_fs_write(JSContext *ctx, JSValueConst this_val, int argc,
                                JSValueConst *argv) {
    (void)this_val;
    if (argc < 2) return JS_ThrowTypeError(ctx, "writeFile requires a path and data");
    const char *path = JS_ToCString(ctx, argv[0]);
    if (!path) return JS_EXCEPTION;

    const uint8_t *data = NULL;
    size_t len = 0;
    const char *as_str = NULL;
    size_t offset = 0, bytes_per = 0;
    JSValue ab = JS_UNDEFINED;

    if (JS_IsString(argv[1])) {
        as_str = JS_ToCStringLen(ctx, &len, argv[1]);
        if (!as_str) {
            JS_FreeCString(ctx, path);
            return JS_EXCEPTION; /* the pending exception is the caller's answer */
        }
        data = (const uint8_t *)as_str;
    } else {
        ab = JS_GetTypedArrayBuffer(ctx, argv[1], &offset, &len, &bytes_per);
        if (JS_IsException(ab)) {
            JS_FreeValue(ctx, ab);
            /* Not a typed array is not an error here — a plain ArrayBuffer is a
             * legitimate argument, and we are about to try it. But the probe left
             * a "not a TypedArray" exception pending on the context (quickjs.c,
             * get_typed_array), and returning a *value* while an exception is set
             * is a contract violation that surfaces later as an unrelated
             * failure. Discard it before taking the second path. */
            JS_FreeValue(ctx, JS_GetException(ctx));
            size_t sz = 0;
            uint8_t *raw = JS_GetArrayBuffer(ctx, &sz, argv[1]);
            if (!raw) {
                JS_FreeCString(ctx, path);
                return JS_ThrowTypeError(ctx, "writeFile data must be a string or bytes");
            }
            data = raw;
            len = sz;
            ab = JS_UNDEFINED;
        } else {
            size_t sz = 0;
            uint8_t *raw = JS_GetArrayBuffer(ctx, &sz, ab);
            if (!raw) {
                JS_FreeValue(ctx, ab);
                JS_FreeCString(ctx, path);
                return JS_ThrowTypeError(ctx, "writeFile data must be a string or bytes");
            }
            data = raw + offset;
        }
    }

    FILE *f = fopen(path, "wb");
    if (!f) {
        if (as_str) JS_FreeCString(ctx, as_str);
        JS_FreeValue(ctx, ab);
        JSValue e = JS_ThrowTypeError(ctx, "cannot write %s", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    size_t wrote = len ? fwrite(data, 1, len, f) : 0;
    fclose(f);
    if (as_str) JS_FreeCString(ctx, as_str);
    JS_FreeValue(ctx, ab);

    if (wrote != len) {
        JSValue e = JS_ThrowTypeError(ctx, "short write to %s", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    JS_FreeCString(ctx, path);
    return JS_UNDEFINED;
}

/* Create a directory and any missing parents, like `mkdir -p`.
 *
 * The recursive form is the one that is actually needed: a tool asked to write
 * "notes/2026/today.md" into an empty workspace has to make two directories, and
 * a non-recursive mkdir would make the tool implement the loop itself against a
 * path syntax it cannot see the root of.
 *
 * Containment is the pre-open's, exactly as for write: this walks the path
 * creating components, and every one of them resolves inside the mount because
 * wazero gave the guest nothing else to resolve against. An existing directory
 * is success, so calling it before every write is cheap and idempotent.
 */
static JSValue js_nine_fs_mkdir(JSContext *ctx, JSValueConst this_val, int argc,
                                JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "mkdir requires a path");
    const char *path = JS_ToCString(ctx, argv[0]);
    if (!path) return JS_EXCEPTION;

    size_t n = strlen(path);
    if (n == 0) {
        JS_FreeCString(ctx, path);
        return JS_ThrowTypeError(ctx, "mkdir requires a non-empty path");
    }

    char *buf = malloc(n + 1);
    if (!buf) {
        JS_FreeCString(ctx, path);
        return JS_ThrowInternalError(ctx, "out of memory");
    }
    memcpy(buf, path, n + 1);

    /* Walk the components, creating each. Start at 1 so a leading '/' is part of
     * the first component rather than an empty one. */
    for (size_t i = 1; i <= n; i++) {
        if (buf[i] != '/' && buf[i] != '\0') continue;
        char saved = buf[i];
        buf[i] = '\0';
        if (mkdir(buf, 0777) != 0 && errno != EEXIST) {
            JSValue e = JS_ThrowTypeError(ctx, "cannot create directory %s", buf);
            free(buf);
            JS_FreeCString(ctx, path);
            return e;
        }
        buf[i] = saved;
    }

    free(buf);
    JS_FreeCString(ctx, path);
    return JS_UNDEFINED;
}

/* List a directory. Names only, no recursion: a tool that wants a tree can walk
 * it, and the flat form is what a pre-open makes cheap and obvious. */
static JSValue js_nine_fs_readdir(JSContext *ctx, JSValueConst this_val, int argc,
                                  JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "readDir requires a path");
    const char *path = JS_ToCString(ctx, argv[0]);
    if (!path) return JS_EXCEPTION;

    DIR *d = opendir(path);
    if (!d) {
        JSValue e = JS_ThrowTypeError(ctx, "cannot read directory %s", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    JSValue arr = JS_NewArray(ctx);
    uint32_t i = 0;
    struct dirent *ent;
    while ((ent = readdir(d)) != NULL) {
        if (strcmp(ent->d_name, ".") == 0 || strcmp(ent->d_name, "..") == 0) continue;
        JS_SetPropertyUint32(ctx, arr, i++, JS_NewString(ctx, ent->d_name));
    }
    closedir(d);
    JS_FreeCString(ctx, path);
    return arr;
}

/* stat, reduced to what a tool acts on: does it exist, how big is it, is it a
 * directory. Mode bits and ownership are not meaningful inside a pre-open. */
static JSValue js_nine_fs_stat(JSContext *ctx, JSValueConst this_val, int argc,
                               JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "stat requires a path");
    const char *path = JS_ToCString(ctx, argv[0]);
    if (!path) return JS_EXCEPTION;

    struct stat st;
    int rc = stat(path, &st);
    JS_FreeCString(ctx, path);
    if (rc != 0) return JS_NULL;

    JSValue o = JS_NewObject(ctx);
    JS_SetPropertyStr(ctx, o, "size", JS_NewInt64(ctx, (int64_t)st.st_size));
    JS_SetPropertyStr(ctx, o, "isDirectory", JS_NewBool(ctx, S_ISDIR(st.st_mode) ? 1 : 0));
    JS_SetPropertyStr(ctx, o, "isFile", JS_NewBool(ctx, S_ISREG(st.st_mode) ? 1 : 0));
    JS_SetPropertyStr(ctx, o, "mtimeMs", JS_NewFloat64(ctx, (double)st.st_mtime * 1000.0));
    return o;
}

/* Read a window of a file: `length` bytes from byte `offset`. Returns a
 * Uint8Array, short at end of file, empty past it.
 *
 * This is what makes a file larger than the interpreter's own memory usable. A
 * tool editing a 200 MB log reads it in windows and never holds more than one;
 * readFile would need the whole thing resident, and the memory cap (16 MiB by
 * default, R-TVM.5) is smaller than the files a workspace holds. */
static JSValue js_nine_fs_read_range(JSContext *ctx, JSValueConst this_val, int argc,
                                     JSValueConst *argv) {
    (void)this_val;
    if (argc < 3) return JS_ThrowTypeError(ctx, "readRange requires a path, offset and length");
    const char *path = JS_ToCString(ctx, argv[0]);
    if (!path) return JS_EXCEPTION;

    int64_t offset = 0, length = 0;
    if (JS_ToInt64(ctx, &offset, argv[1]) || JS_ToInt64(ctx, &length, argv[2])) {
        JS_FreeCString(ctx, path);
        return JS_EXCEPTION;
    }
    if (offset < 0) offset = 0;
    if (length <= 0) {
        JS_FreeCString(ctx, path);
        return JS_NewUint8ArrayCopy(ctx, NULL, 0);
    }

    FILE *f = fopen(path, "rb");
    if (!f) {
        JSValue e = JS_ThrowTypeError(ctx, "cannot read %s", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    if (fseek(f, (long)offset, SEEK_SET) != 0) {
        fclose(f);
        JS_FreeCString(ctx, path);
        /* Seeking past the end is not an error on every platform; an empty
         * window terminates a paging loop cleanly either way. */
        return JS_NewUint8ArrayCopy(ctx, NULL, 0);
    }

    uint8_t *buf = malloc((size_t)length);
    if (!buf) { fclose(f); JS_FreeCString(ctx, path); return JS_ThrowOutOfMemory(ctx); }

    size_t got = fread(buf, 1, (size_t)length, f);
    int failed = ferror(f);
    fclose(f);
    if (failed) {
        free(buf);
        JSValue e = JS_ThrowTypeError(ctx, "error reading %s", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    JS_FreeCString(ctx, path);

    JSValue out = JS_NewUint8ArrayCopy(ctx, buf, got);
    free(buf);
    return out;
}

/* Append to a file, creating it when absent. Accepts a string (UTF-8) or any
 * ArrayBuffer/view, matching writeFile. Appending is not writeFile with extra
 * steps: a tool adding a line to a log would otherwise read the whole file back
 * and rewrite it, which is both the memory problem above and a window in which
 * an interrupted write loses what was already there. */
static JSValue js_nine_fs_append(JSContext *ctx, JSValueConst this_val, int argc,
                                 JSValueConst *argv) {
    (void)this_val;
    if (argc < 2) return JS_ThrowTypeError(ctx, "appendFile requires a path and data");
    const char *path = JS_ToCString(ctx, argv[0]);
    if (!path) return JS_EXCEPTION;

    const uint8_t *data = NULL;
    size_t len = 0;
    const char *as_str = NULL;
    size_t offset = 0, bytes_per = 0;
    JSValue ab = JS_UNDEFINED;

    if (JS_IsString(argv[1])) {
        as_str = JS_ToCStringLen(ctx, &len, argv[1]);
        if (!as_str) { JS_FreeCString(ctx, path); return JS_EXCEPTION; }
        data = (const uint8_t *)as_str;
    } else {
        ab = JS_GetTypedArrayBuffer(ctx, argv[1], &offset, &len, &bytes_per);
        if (JS_IsException(ab)) {
            JS_FreeValue(ctx, ab);
            size_t ab_len = 0;
            uint8_t *raw = JS_GetArrayBuffer(ctx, &ab_len, argv[1]);
            if (!raw) {
                JS_FreeCString(ctx, path);
                return JS_ThrowTypeError(ctx, "appendFile: data must be a string or binary");
            }
            data = raw;
            len = ab_len;
        } else {
            size_t ab_len = 0;
            uint8_t *raw = JS_GetArrayBuffer(ctx, &ab_len, ab);
            JS_FreeValue(ctx, ab);
            if (!raw) {
                JS_FreeCString(ctx, path);
                return JS_ThrowTypeError(ctx, "appendFile: data must be a string or binary");
            }
            data = raw + offset;
        }
    }

    FILE *f = fopen(path, "ab");
    if (!f) {
        if (as_str) JS_FreeCString(ctx, as_str);
        JSValue e = JS_ThrowTypeError(ctx, "cannot append to %s", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    size_t wrote = len ? fwrite(data, 1, len, f) : 0;
    int failed = fclose(f) != 0 || wrote != len;
    if (as_str) JS_FreeCString(ctx, as_str);
    if (failed) {
        JSValue e = JS_ThrowTypeError(ctx, "error appending to %s", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    JS_FreeCString(ctx, path);
    return JS_NewInt64(ctx, (int64_t)len);
}

/* Rename within the pre-open. This is the primitive behind three things a tool
 * could not do at all: relocating a file without copying it, replacing a file
 * atomically (write a temporary, rename over the target), and moving a deleted
 * file into a trash directory instead of destroying it. All three are one inode
 * operation, whatever the file's size. */
static JSValue js_nine_fs_rename(JSContext *ctx, JSValueConst this_val, int argc,
                                 JSValueConst *argv) {
    (void)this_val;
    if (argc < 2) return JS_ThrowTypeError(ctx, "rename requires a source and destination");
    const char *from = JS_ToCString(ctx, argv[0]);
    if (!from) return JS_EXCEPTION;
    const char *to = JS_ToCString(ctx, argv[1]);
    if (!to) { JS_FreeCString(ctx, from); return JS_EXCEPTION; }

    int rc = rename(from, to);
    if (rc != 0) {
        JSValue e = JS_ThrowTypeError(ctx, "cannot rename %s to %s", from, to);
        JS_FreeCString(ctx, from);
        JS_FreeCString(ctx, to);
        return e;
    }
    JS_FreeCString(ctx, from);
    JS_FreeCString(ctx, to);
    return JS_UNDEFINED;
}

/* Remove one file or one empty directory. Not recursive: a tool that means to
 * delete a tree must walk it and say so at every step, and nothing here can
 * erase a directory by accident. */
static JSValue js_nine_fs_unlink(JSContext *ctx, JSValueConst this_val, int argc,
                                 JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "remove requires a path");
    const char *path = JS_ToCString(ctx, argv[0]);
    if (!path) return JS_EXCEPTION;

    struct stat st;
    if (stat(path, &st) != 0) {
        JSValue e = JS_ThrowTypeError(ctx, "cannot remove %s: no such file", path);
        JS_FreeCString(ctx, path);
        return e;
    }
    int rc = S_ISDIR(st.st_mode) ? rmdir(path) : unlink(path);
    if (rc != 0) {
        JSValue e = JS_ThrowTypeError(ctx, S_ISDIR(st.st_mode)
                                               ? "cannot remove directory %s (is it empty?)"
                                               : "cannot remove %s",
                                      path);
        JS_FreeCString(ctx, path);
        return e;
    }
    JS_FreeCString(ctx, path);
    return JS_UNDEFINED;
}

/* One environment variable. The host passes only the keys the operator named, so
 * this cannot observe a key that was not granted — the filtering is wazero's, not
 * a check here that could be wrong. */
static JSValue js_nine_env(JSContext *ctx, JSValueConst this_val, int argc,
                           JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "get requires a name");
    const char *name = JS_ToCString(ctx, argv[0]);
    if (!name) return JS_EXCEPTION;
    const char *v = getenv(name);
    JS_FreeCString(ctx, name);
    return v ? JS_NewString(ctx, v) : JS_UNDEFINED;
}

/* Cryptographic randomness. getentropy is wasi-libc's wrapper over WASI
 * random_get, which the host feeds from crypto/rand — so this is a real CSPRNG
 * and not Math.random with a better name. */
static JSValue js_nine_random(JSContext *ctx, JSValueConst this_val, int argc,
                              JSValueConst *argv) {
    (void)this_val;
    if (argc < 1) return JS_ThrowTypeError(ctx, "randomBytes requires a length");
    int64_t n = 0;
    if (JS_ToInt64(ctx, &n, argv[0])) return JS_EXCEPTION;
    if (n < 0 || n > 65536) {
        return JS_ThrowRangeError(ctx, "randomBytes length must be 0..65536");
    }
    uint8_t *buf = malloc((size_t)n ? (size_t)n : 1);
    if (!buf) return JS_ThrowOutOfMemory(ctx);
    /* getentropy caps at 256 bytes per call. */
    for (int64_t off = 0; off < n; off += 256) {
        size_t chunk = (size_t)(n - off < 256 ? n - off : 256);
        if (getentropy(buf + off, chunk) != 0) {
            free(buf);
            return JS_ThrowInternalError(ctx, "randomness is unavailable");
        }
    }
    JSValue out = JS_NewUint8ArrayCopy(ctx, buf, (size_t)n);
    free(buf);
    return out;
}

/* The calling tool's grant, as a JSON string. */
static JSValue js_nine_caps(JSContext *ctx, JSValueConst this_val, int argc,
                            JSValueConst *argv) {
    (void)this_val; (void)argc; (void)argv;
    uint64_t packed = nine_host_caps();
    if (packed == 0) return JS_NewString(ctx, "{}");
    const char *out = (const char *)(uintptr_t)(uint32_t)(packed >> 32);
    uint32_t out_len = (uint32_t)(packed & 0xffffffff);
    JSValue res = JS_NewStringLen(ctx, out, out_len);
    free((void *)out);
    return res;
}

/* ── error reporting ─────────────────────────────────────────────────────── */

/* Render the pending exception as the ABI's failure envelope. Building it with
 * JS_JSONStringify rather than sprintf means a message containing quotes or
 * newlines — a syntax error quoting the offending line, say — cannot corrupt
 * the JSON the host is about to parse. */
/* ── the work budget ──────────────────────────────────────────────────────
 *
 * The wall clock bounds how long a tool runs; this bounds how much it *does*.
 * The difference matters because a deadline is a property of the machine — the
 * same tool passes on a fast host and fails on a loaded one — while a work
 * budget is a property of the tool.
 *
 * QuickJS calls the interrupt handler once every JS_INTERRUPT_COUNTER_INIT
 * polls, and a poll happens at a backward jump or a call: loop iterations and
 * function calls, not every bytecode op. So the unit here is *checks*, and the
 * host converts the operator's "operations" into them — the conversion is
 * approximate by nature and belongs where it can be named, not in this file.
 *
 * Returning non-zero makes QuickJS throw an **uncatchable** InternalError
 * (JS_ThrowInterrupted calls JS_SetUncatchableError), which is what makes this
 * a bound rather than a suggestion: the unwinder skips every `catch` for such an
 * error and an async function propagates it instead of rejecting, so a tool
 * cannot wrap its loop in try/catch and keep going.
 *
 * Exhaustion is sticky. Once the budget is gone every later check fails too, so
 * nothing resumes after the throw.
 */
static int g_budget_on = 0;
static uint32_t g_checks_left = 0;
static uint32_t g_checks_per_trigger = 0;
static int g_budget_spent = 0;

/* A live process runs until stopped, so its budget bounds the work done per
 * trigger, not over its whole life. The host calls this when nine:process's
 * next() hands the guest a new trigger — and only then: a loop around report()
 * or a model turn does not refill it. An exhausted budget stays exhausted. */
__attribute__((export_name("nine_budget_reset"))) void nine_budget_reset(void) {
    if (g_budget_on && !g_budget_spent) g_checks_left = g_checks_per_trigger;
}

static int nine_interrupt(JSRuntime *rt, void *opaque) {
    (void)rt;
    (void)opaque;
    if (!g_budget_on) return 0;
    if (g_budget_spent) return 1;
    if (g_checks_left > 0) {
        g_checks_left--;
        return 0;
    }
    g_budget_spent = 1;
    return 1;
}

/* The budget failure, as an ordinary tool error carrying a stable code. The
 * message the operator reads is the host's to write: it knows the configured
 * number and the key to raise, and this file knows neither. */
static uint64_t budget_failure(void) {
    return pack_owned(
        "{\"ok\":false,\"error\":\"exceeded its work budget\","
        "\"error_detail\":{\"code\":\"E_WORK_BUDGET\",\"retryable\":false}}");
}

static uint64_t fail(JSContext *ctx) {
    /* An exhausted budget reports as itself, whatever the interpreter happened
     * to throw on the way out. Every failure path funnels through here. */
    if (g_budget_spent) return budget_failure();

    JSValue exc = JS_GetException(ctx);

    JSValue msg = JS_UNDEFINED;
    if (JS_IsError(exc)) msg = JS_GetPropertyStr(ctx, exc, "message");
    if (!JS_IsString(msg)) {
        JS_FreeValue(ctx, msg);
        msg = JS_ToString(ctx, exc);
    }

    JSValue out = JS_NewObject(ctx);
    JS_SetPropertyStr(ctx, out, "ok", JS_FALSE);
    JS_SetPropertyStr(ctx, out, "error",
                      JS_IsString(msg) ? msg : JS_NewString(ctx, "unknown error"));

    JSValue json = JS_JSONStringify(ctx, out, JS_UNDEFINED, JS_UNDEFINED);
    JS_FreeValue(ctx, out);
    JS_FreeValue(ctx, exc);

    const char *s = JS_ToCString(ctx, json);
    uint64_t packed = pack_owned(s ? s : "{\"ok\":false,\"error\":\"unknown error\"}");
    if (s) JS_FreeCString(ctx, s);
    JS_FreeValue(ctx, json);
    return packed;
}

/* ── ABI: the call ────────────────────────────────────────────────────────
 *
 * Input is the envelope the host wrote at `ptr`:
 *
 *   { "modules": { "<specifier>": "<js>", ... },
 *     "args": <the model's arguments>,
 *     "job": { "cursor": "<opaque>", "call": <n> },
 *     "checks": <the work budget, in interrupt checks; absent is unmetered> }
 *
 * The harness is not in it: the host installs it separately, as bytecode,
 * through nine_harness above.
 *
 * `job` is present only when the host is running this tool as a long-running
 * job; an ordinary call omits it and the harness passes undefined.
 *
 * `modules` always carries the tool's own source under the specifier the
 * harness imports; the rest is whatever the allowlist admitted. Output is
 * {"ok":true,"output":"..."} or {"ok":false,"error":"..."}.
 *
 * A fresh runtime per call is the point, not an accident (§3): no global, no
 * cached credential, no poisoned prototype survives to be observed by the next
 * call. Nothing here is reused between invocations.
 */
__attribute__((export_name("nine_run"))) uint64_t nine_run(uint32_t ptr, uint32_t len) {
    JSRuntime *rt = JS_NewRuntime();
    if (!rt) return pack_owned("{\"ok\":false,\"error\":\"cannot create runtime\"}");
    JSContext *ctx = JS_NewContext(rt);
    if (!ctx) {
        JS_FreeRuntime(rt);
        return pack_owned("{\"ok\":false,\"error\":\"cannot create context\"}");
    }

    uint64_t result;
    JSValue envelope = JS_ParseJSON(ctx, (const char *)(uintptr_t)ptr, len, "<envelope>");
    if (JS_IsException(envelope)) {
        result = fail(ctx);
        goto done;
    }

    /* Install the closed resolver before anything is evaluated, so there is no
     * window in which the default path-resolving loader is live. */
    static nine_modules set;
    set.modules = JS_GetPropertyStr(ctx, envelope, "modules");
    if (!JS_IsObject(set.modules)) {
        JS_FreeValue(ctx, set.modules);
        set.modules = JS_NewObject(ctx);
    }
    JS_SetModuleLoaderFunc(rt, nine_module_normalize, nine_module_load, &set);

    /* The work budget, installed before anything the envelope named is
     * evaluated — the harness included, so no code runs unmetered. The statics
     * are reset rather than trusted: this instance is fresh (§3), but a bound
     * that depends on that being true is a bound waiting to be wrong. */
    g_budget_on = 0;
    g_budget_spent = 0;
    g_checks_left = 0;
    JSValue checks = JS_GetPropertyStr(ctx, envelope, "checks");
    if (JS_IsNumber(checks)) {
        uint32_t n = 0;
        if (JS_ToUint32(ctx, &n, checks) == 0 && n > 0) {
            g_budget_on = 1;
            g_checks_left = n;
            g_checks_per_trigger = n;
            JS_SetInterruptHandler(rt, nine_interrupt, NULL);
        }
    }
    JS_FreeValue(ctx, checks);

    /* The harness reads its arguments and writes its result through these two
     * globals. Passing arguments as a parsed value rather than a string spares
     * the guest a redundant JSON round-trip on every call. */
    JSValue global = JS_GetGlobalObject(ctx);
    JSValue args = JS_GetPropertyStr(ctx, envelope, "args");
    JS_SetPropertyStr(ctx, global, "__nine_args", args);

    /* The job context, when this call is one of a long-running sequence. Absent
     * on an ordinary call, which is the overwhelming majority — the harness
     * hands the tool undefined and nothing changes for a tool that never asked
     * to be resumable. */
    JSValue job = JS_GetPropertyStr(ctx, envelope, "job");
    JS_SetPropertyStr(ctx, global, "__nine_job", job);
    JS_SetPropertyStr(ctx, global, "__nine_result", JS_UNDEFINED);
    JS_SetPropertyStr(ctx, global, "__nine_log",
                      JS_NewCFunction(ctx, js_nine_log, "__nine_log", 1));
    JS_SetPropertyStr(ctx, global, "__nine_http",
                      JS_NewCFunction(ctx, js_nine_http, "__nine_http", 1));
    JS_SetPropertyStr(ctx, global, "__nine_caps",
                      JS_NewCFunction(ctx, js_nine_caps, "__nine_caps", 0));
    JS_SetPropertyStr(ctx, global, "__nine_state",
                      JS_NewCFunction(ctx, js_nine_state, "__nine_state", 1));
    JS_SetPropertyStr(ctx, global, "__nine_fs_read",
                      JS_NewCFunction(ctx, js_nine_fs_read, "__nine_fs_read", 1));
    JS_SetPropertyStr(ctx, global, "__nine_fs_write",
                      JS_NewCFunction(ctx, js_nine_fs_write, "__nine_fs_write", 2));
    JS_SetPropertyStr(ctx, global, "__nine_fs_mkdir",
                      JS_NewCFunction(ctx, js_nine_fs_mkdir, "__nine_fs_mkdir", 1));
    JS_SetPropertyStr(ctx, global, "__nine_fs_readdir",
                      JS_NewCFunction(ctx, js_nine_fs_readdir, "__nine_fs_readdir", 1));
    JS_SetPropertyStr(ctx, global, "__nine_fs_stat",
                      JS_NewCFunction(ctx, js_nine_fs_stat, "__nine_fs_stat", 1));
    JS_SetPropertyStr(ctx, global, "__nine_fs_read_range",
                      JS_NewCFunction(ctx, js_nine_fs_read_range, "__nine_fs_read_range", 3));
    JS_SetPropertyStr(ctx, global, "__nine_fs_append",
                      JS_NewCFunction(ctx, js_nine_fs_append, "__nine_fs_append", 2));
    JS_SetPropertyStr(ctx, global, "__nine_fs_rename",
                      JS_NewCFunction(ctx, js_nine_fs_rename, "__nine_fs_rename", 2));
    JS_SetPropertyStr(ctx, global, "__nine_fs_unlink",
                      JS_NewCFunction(ctx, js_nine_fs_unlink, "__nine_fs_unlink", 1));
    JS_SetPropertyStr(ctx, global, "__nine_env",
                      JS_NewCFunction(ctx, js_nine_env, "__nine_env", 1));
    JS_SetPropertyStr(ctx, global, "__nine_random",
                      JS_NewCFunction(ctx, js_nine_random, "__nine_random", 1));
    JS_SetPropertyStr(ctx, global, "__nine_process",
                      JS_NewCFunction(ctx, js_nine_process, "__nine_process", 1));
    JS_FreeValue(ctx, global);

    if (!g_harness || g_harness_len == 0) {
        result = pack_owned(
            "{\"ok\":false,\"error\":\"the host did not install a harness\"}");
        goto done;
    }

    /* Reading bytecode is only safe for input the host controls, which this is:
     * it is built from harness.js at build time and embedded in the daemon, and
     * a tool never reaches it. Everything a tool supplies is still source. */
    JSValue mod = JS_ReadObject(ctx, g_harness, g_harness_len, JS_READ_OBJ_BYTECODE);
    if (JS_IsException(mod)) {
        result = fail(ctx);
        goto done;
    }
    /* Links the harness's `nine:tool` import through the resolver installed
     * above, exactly as evaluating the source did. */
    if (JS_ResolveModule(ctx, mod) < 0) {
        JS_FreeValue(ctx, mod);
        result = fail(ctx);
        goto done;
    }

    JSValue ev = JS_EvalFunction(ctx, mod); /* consumes mod */
    if (JS_IsException(ev)) {
        JS_FreeValue(ctx, ev);
        result = fail(ctx);
        goto done;
    }

    /* Drain the microtask queue so a tool that returns a promise, or uses
     * top-level await, resolves before we read its result. The wall-clock
     * deadline is the bound here: a job queue that never empties is closed out
     * from under us by the host (§3), which is the only CPU bound wazero
     * offers. */
    for (;;) {
        JSContext *pctx = NULL;
        int n = JS_ExecutePendingJob(rt, &pctx);
        if (n <= 0) {
            if (n < 0 && pctx) {
                JS_FreeValue(ctx, ev);
                result = fail(pctx);
                goto done;
            }
            break;
        }
    }

    /* A module evaluation yields a promise; a rejection there is the tool's
     * failure and must not be reported as an empty result. */
    if (JS_PromiseState(ctx, ev) == JS_PROMISE_REJECTED) {
        JSValue reason = JS_PromiseResult(ctx, ev);
        JS_Throw(ctx, reason);
        JS_FreeValue(ctx, ev);
        result = fail(ctx);
        goto done;
    }
    JS_FreeValue(ctx, ev);

    global = JS_GetGlobalObject(ctx);
    JSValue out = JS_GetPropertyStr(ctx, global, "__nine_result");
    JS_FreeValue(ctx, global);

    if (!JS_IsString(out)) {
        JS_FreeValue(ctx, out);
        /* An uncatchable interrupt unwinds past the harness's own error
         * handling, so a spent budget arrives here as a missing result rather
         * than as an exception. */
        result = g_budget_spent
                     ? budget_failure()
                     : pack_owned("{\"ok\":false,\"error\":\"tool produced no result\"}");
        goto done;
    }

    const char *s = JS_ToCString(ctx, out);
    result = pack_owned(s ? s : "{\"ok\":false,\"error\":\"unreadable result\"}");
    if (s) JS_FreeCString(ctx, s);
    JS_FreeValue(ctx, out);

done:
    /* Deliberately not freeing the context and runtime: the module instance is
     * discarded whole by the host the moment this returns, and tearing down a
     * QuickJS heap we are about to throw away only burns deadline. */
    return result;
}
