// harness.js — the `js` tool kind's entry module.
//
// This is the "few lines wrapping the author's default export" of
// docs/sandboxed-tools.md §4. It is evaluated by qjs_host.c as the entry
// module; the tool's own source is served to its `import` by the host-side
// allowlist resolver (§4.3) under the fixed specifier below.
//
// It exists so that the C shim stays dumb: everything that needs to know what a
// Nine tool *is* — that it default-exports a function, that a string result
// passes through and anything else is stringified — lives here, in JavaScript,
// where it is legible.

import tool from "nine:tool";

// The `log` capability (§6.2), granted by default. QuickJS itself has no
// console: the stock one comes from quickjs-libc's js_std_add_helpers, which
// this build does not link (§4.1). So the surface an author expects is built
// here on top of the single host import, and it is the only way out of the
// sandbox that a tool has without a grant.
const write = (args) =>
  __nine_log(
    args
      .map((a) => (typeof a === "string" ? a : safeStringify(a)))
      .join(" "),
  );
globalThis.console = {
  log: (...a) => write(a),
  info: (...a) => write(a),
  warn: (...a) => write(a),
  error: (...a) => write(a),
  debug: (...a) => write(a),
};

function safeStringify(v) {
  try {
    return JSON.stringify(v) ?? String(v);
  } catch {
    return String(v); // cycles, BigInt, and anything else JSON refuses
  }
}

// Everything crossing the ABI boundary is a UTF-8 JSON byte slice (§4), and the
// contract downstream is CallResult{Output string} — so a string result is the
// tool's own formatting and passes through untouched, while any other value is
// serialized. Returning an object is the common case and should not require the
// author to remember to stringify it.
function render(value) {
  if (typeof value === "string") return value;
  if (value === undefined || value === null) return "";
  return safeStringify(value);
}

try {
  if (typeof tool !== "function") {
    throw new TypeError(
      "tool must default-export a function: export default (args) => ...",
    );
  }
  // Awaited so an async tool works without ceremony. qjs_host.c drains the
  // microtask queue before reading the result, and the wall-clock deadline
  // bounds a promise that never settles.
  const value = await tool(globalThis.__nine_args);
  globalThis.__nine_result = JSON.stringify({ ok: true, output: render(value) });
} catch (e) {
  globalThis.__nine_result = JSON.stringify({
    ok: false,
    error: e && e.message ? String(e.message) : String(e),
  });
}
