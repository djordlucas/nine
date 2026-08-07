/*
 * upper.c — a raw `wasm` tool, in C, built by testdata/build.sh.
 *
 * Its whole purpose is to prove that the Nine ABI is genuinely
 * language-agnostic rather than QuickJS-shaped: this module knows nothing about
 * JavaScript, links no interpreter, and satisfies the same contract a Rust,
 * TinyGo, or Zig tool would (docs/sandboxed-tools.md §4). It is also the
 * smallest complete example of the ABI, so the authoring guide points at it.
 *
 * It takes {"text": "..."} and returns the text uppercased.
 */

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

__attribute__((export_name("nine_alloc"))) uint32_t nine_alloc(uint32_t size) {
    return (uint32_t)(uintptr_t)malloc(size ? size : 1);
}

/* A deliberately minimal JSON reach-in: find "text" and copy its string value.
 * A real tool would use a parser; this one stays dependency-free so the built
 * artifact is a few hundred bytes and its behavior is obvious by inspection. */
static int find_text(const char *in, uint32_t len, const char **out, uint32_t *out_len) {
    const char *key = "\"text\"";
    for (uint32_t i = 0; i + 6 <= len; i++) {
        if (memcmp(in + i, key, 6) != 0) continue;
        uint32_t j = i + 6;
        while (j < len && (in[j] == ' ' || in[j] == ':')) j++;
        if (j >= len || in[j] != '"') return 0;
        j++;
        uint32_t start = j;
        while (j < len && in[j] != '"') j++;
        *out = in + start;
        *out_len = j - start;
        return 1;
    }
    return 0;
}

__attribute__((export_name("nine_run"))) uint64_t nine_run(uint32_t ptr, uint32_t len) {
    const char *in = (const char *)(uintptr_t)ptr;

    const char *text = NULL;
    uint32_t text_len = 0;
    if (!find_text(in, len, &text, &text_len)) {
        const char *err = "{\"ok\":false,\"error\":\"expected a string argument 'text'\"}";
        uint32_t n = (uint32_t)strlen(err);
        char *buf = malloc(n);
        memcpy(buf, err, n);
        return ((uint64_t)(uint32_t)(uintptr_t)buf << 32) | n;
    }

    const char *pre = "{\"ok\":true,\"output\":\"";
    const char *post = "\"}";
    uint32_t pre_n = (uint32_t)strlen(pre), post_n = (uint32_t)strlen(post);
    char *buf = malloc(pre_n + text_len + post_n);
    memcpy(buf, pre, pre_n);
    for (uint32_t i = 0; i < text_len; i++) {
        char c = text[i];
        buf[pre_n + i] = (c >= 'a' && c <= 'z') ? (char)(c - 32) : c;
    }
    memcpy(buf + pre_n + text_len, post, post_n);

    uint32_t total = pre_n + text_len + post_n;
    return ((uint64_t)(uint32_t)(uintptr_t)buf << 32) | total;
}
