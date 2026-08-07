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

// The `net.http` capability (§8), shaped like the `fetch` every JS author
// already knows. It is a faithful *subset*, not a polyfill: there is no
// streaming, no AbortController, no cookie jar, no redirect control, and no
// Request/Headers classes. What is here behaves as expected.
//
// Note what this function does NOT do: decide anything. Method allowlists, host
// allowlists, SSRF rejection on the resolved IP, per-redirect revalidation, and
// response caps are all enforced on the host side of __nine_http, where a tool
// cannot reach them. A tool without the grant gets a refusal here, the same as
// a tool that aimed at a blocked address — so neither is a special case it can
// probe for.
//
// It is defined unconditionally because the underlying import must exist for the
// module to instantiate at all; permission is checked per call by the host.
globalThis.fetch = async (url, init = {}) => {
  const raw = __nine_http(
    JSON.stringify({
      url: String(url),
      method: init.method ?? "GET",
      headers: init.headers ?? {},
      body: init.body == null ? "" : String(init.body),
    }),
  );

  const res = JSON.parse(raw);
  if (res.error) {
    // A refusal is a thrown error rather than a status code: it is not a
    // response, and letting it look like one invites `if (res.ok)` to swallow a
    // policy decision the operator made.
    throw new Error(res.error);
  }

  const body = res.body ?? "";
  return {
    status: res.status,
    ok: res.status >= 200 && res.status < 300,
    headers: res.headers ?? {},
    text: () => body,
    json: () => JSON.parse(body),
  };
};

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
