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
// Bytes cross the boundary base64-encoded, because everything crossing it is
// UTF-8 JSON and a JSON string cannot hold arbitrary bytes. atob/btoa are the
// right primitives for that here despite their reputation: they are Latin-1, i.e.
// byte-oriented, which is exactly what is wanted when the payload is bytes rather
// than text. (Their reputation comes from being used on *text*, where Latin-1
// silently mangles anything non-ASCII.)
function bytesToB64(u8) {
  let s = "";
  // Chunked to avoid a huge apply() argument list on a 1 MiB body.
  for (let i = 0; i < u8.length; i += 8192) {
    s += String.fromCharCode.apply(null, u8.subarray(i, i + 8192));
  }
  return btoa(s);
}

function b64ToBytes(b64) {
  const s = atob(b64);
  const u8 = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) u8[i] = s.charCodeAt(i);
  return u8;
}

// Minimal UTF-8 encoder, so bytes() works on a text response too. TextEncoder
// proper is M4 (docs/rich-js-tools.md §6.5); this is the private subset needed
// to keep bytes() from having a hole in it.
function utf8Encode(str) {
  const out = [];
  for (const ch of str) {
    let c = ch.codePointAt(0);
    if (c < 0x80) out.push(c);
    else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 0x3f));
    else if (c < 0x10000)
      out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f));
    else
      out.push(
        0xf0 | (c >> 18),
        0x80 | ((c >> 12) & 0x3f),
        0x80 | ((c >> 6) & 0x3f),
        0x80 | (c & 0x3f),
      );
  }
  return new Uint8Array(out);
}

globalThis.fetch = async (url, init = {}) => {
  const req = {
    url: String(url),
    method: init.method ?? "GET",
    headers: init.headers ?? {},
    body: "",
  };

  // A binary body used to be String()-ed, which put the characters "1,2,3,255"
  // on the wire for a Uint8Array — wrong, and silently so.
  const b = init.body;
  if (b == null) {
    req.body = "";
  } else if (b instanceof Uint8Array) {
    req.body_b64 = bytesToB64(b);
  } else if (b instanceof ArrayBuffer) {
    req.body_b64 = bytesToB64(new Uint8Array(b));
  } else if (ArrayBuffer.isView(b)) {
    req.body_b64 = bytesToB64(
      new Uint8Array(b.buffer, b.byteOffset, b.byteLength),
    );
  } else {
    req.body = String(b);
  }

  const res = JSON.parse(__nine_http(JSON.stringify(req)));
  if (res.error) {
    // A refusal is a thrown error rather than a status code: it is not a
    // response, and letting it look like one invites `if (res.ok)` to swallow a
    // policy decision the operator made.
    throw new Error(res.error);
  }

  // Exactly one of these is set by the host: body for a valid-UTF-8 response,
  // body_b64 for anything else. The binary case used to arrive as body with every
  // invalid byte replaced by U+FFFD, and nothing said so.
  const isBinary = typeof res.body_b64 === "string";
  const body = res.body ?? "";

  return {
    status: res.status,
    ok: res.status >= 200 && res.status < 300,
    headers: res.headers ?? {},
    // Throwing beats returning mojibake. A tool that asks for text and gets a
    // PNG has a bug, and the bug should surface here rather than three
    // transformations later as a wrong answer.
    text: () => {
      if (isBinary) {
        throw new Error(
          "response body is not valid UTF-8 text; use bytes() or arrayBuffer()",
        );
      }
      return body;
    },
    json: () => {
      if (isBinary) {
        throw new Error(
          "response body is not valid UTF-8 text and cannot be JSON; use bytes()",
        );
      }
      return JSON.parse(body);
    },
    bytes: () => (isBinary ? b64ToBytes(res.body_b64) : utf8Encode(body)),
    arrayBuffer: () => {
      const u8 = isBinary ? b64ToBytes(res.body_b64) : utf8Encode(body);
      // Sliced so the caller cannot reach past its own view into whatever else
      // the decoder's buffer happens to hold.
      return u8.buffer.slice(u8.byteOffset, u8.byteOffset + u8.byteLength);
    },
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

// The structured half of a failure (§6.1 of docs/rich-js-tools.md). A thrown
// Error already carries more than a sentence — its class, a `code` by widespread
// convention, and since ES2022 a `cause` chain — and all of it used to be
// discarded here in favor of `.message` alone. That made "your argument was
// malformed" and "the upstream is down" the same instruction to the model.
//
// Every field is optional and omitted when absent, so a tool that throws a plain
// Error produces exactly the envelope it produced before.
function detail(e) {
  if (!e || typeof e !== "object") return undefined;
  const d = {};

  if (typeof e.name === "string" && e.name) d.name = e.name;
  // `code` is not standard, but it is the convention across Node, V8, and most
  // libraries, and a tool author reaching for a stable identifier reaches for it.
  if (typeof e.code === "string" && e.code) d.code = e.code;
  else if (typeof e.code === "number") d.code = String(e.code);
  if (typeof e.retryable === "boolean") d.retryable = e.retryable;

  // Walk the cause chain, outermost first. Bounded because a cycle here would
  // otherwise spin until the wall clock kills the call — and a self-referential
  // cause is a bug in the tool, not something to hang on.
  const cause = [];
  let seen = new Set([e]);
  let cur = e.cause;
  while (cur != null && cause.length < 8 && !seen.has(cur)) {
    seen.add(cur);
    cause.push(
      typeof cur === "object" && cur.message ? String(cur.message) : String(cur),
    );
    cur = typeof cur === "object" ? cur.cause : undefined;
  }
  if (cause.length) d.cause = cause;

  return Object.keys(d).length ? d : undefined;
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
    error_detail: detail(e),
  });
}
