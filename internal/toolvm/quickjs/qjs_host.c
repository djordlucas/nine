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

#include "quickjs.h"

/* ── the one host import ──────────────────────────────────────────────────
 * The `log` capability (§6.2). Granted by default because it leaks nothing:
 * the daemon decides where the bytes go (slog + the event journal).
 */
__attribute__((import_module("nine"), import_name("log"))) extern void
nine_host_log(const uint8_t *ptr, int32_t len);

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

/* ── error reporting ─────────────────────────────────────────────────────── */

/* Render the pending exception as the ABI's failure envelope. Building it with
 * JS_JSONStringify rather than sprintf means a message containing quotes or
 * newlines — a syntax error quoting the offending line, say — cannot corrupt
 * the JSON the host is about to parse. */
static uint64_t fail(JSContext *ctx) {
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
 *   { "harness": "<js>", "modules": { "<specifier>": "<js>", ... },
 *     "args": <the model's arguments> }
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

    /* The harness reads its arguments and writes its result through these two
     * globals. Passing arguments as a parsed value rather than a string spares
     * the guest a redundant JSON round-trip on every call. */
    JSValue global = JS_GetGlobalObject(ctx);
    JSValue args = JS_GetPropertyStr(ctx, envelope, "args");
    JS_SetPropertyStr(ctx, global, "__nine_args", args);
    JS_SetPropertyStr(ctx, global, "__nine_result", JS_UNDEFINED);
    JS_SetPropertyStr(ctx, global, "__nine_log",
                      JS_NewCFunction(ctx, js_nine_log, "__nine_log", 1));
    JS_FreeValue(ctx, global);

    JSValue harness = JS_GetPropertyStr(ctx, envelope, "harness");
    size_t hlen = 0;
    const char *hsrc = JS_ToCStringLen(ctx, &hlen, harness);
    JS_FreeValue(ctx, harness);
    if (!hsrc) {
        result = fail(ctx);
        goto done;
    }

    JSValue ev = JS_Eval(ctx, hsrc, hlen, "nine:harness", JS_EVAL_TYPE_MODULE);
    JS_FreeCString(ctx, hsrc);
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
        result = pack_owned("{\"ok\":false,\"error\":\"tool produced no result\"}");
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
