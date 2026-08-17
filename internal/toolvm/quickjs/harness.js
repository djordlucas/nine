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
//
// It is also where the web-platform layer lives (docs/rich-js-tools.md §6.5).
// QuickJS implements ECMAScript, and ECMAScript has no TextEncoder, URL,
// structuredClone, or timers — those are the platform around the language, and
// on a browser or in Node they come from the host. Here they come from this file,
// which is embedded source rather than part of the committed qjs.wasm, so adding
// to them costs a `make build` and not a reviewed binary diff.
//
// IMPORTANT — the tool is imported *dynamically*, at the bottom. A static import
// is hoisted and evaluated before this module's body, which meant a tool's
// module-level `console.log` ran before console existed and died with
// "console is not defined" — the first thing anyone writes when debugging.

// The host bindings, captured into closures and then removed from the global
// object. They are ours, not the author's: leaving them enumerable on globalThis
// collided with author code and invited tools to bind to internals we want to be
// free to change. `__nine_result` is the exception and has to stay — qjs_host.c
// reads the result back off the global object after evaluation.
const hostLog = globalThis.__nine_log;
const hostHTTP = globalThis.__nine_http;
const toolArgs = globalThis.__nine_args;
delete globalThis.__nine_log;
delete globalThis.__nine_http;
delete globalThis.__nine_args;

// The `log` capability (§6.2), granted by default. QuickJS itself has no
// console: the stock one comes from quickjs-libc's js_std_add_helpers, which
// this build does not link (§4.1). So the surface an author expects is built
// here on top of the single host import, and it is the only way out of the
// sandbox that a tool has without a grant.
const write = (args) =>
  hostLog(
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
// ── the web-platform layer ────────────────────────────────────────────────
// Everything below is absent from ECMAScript and present everywhere JavaScript
// actually runs. Each is a deliberate subset, and each says so.

// TextEncoder / TextDecoder, UTF-8 only. There is no ICU here and no other
// encoding is worth pretending to support: a `new TextDecoder("shift_jis")` that
// silently produced UTF-8 would be worse than one that refuses.
class TextEncoder {
  get encoding() {
    return "utf-8";
  }
  encode(str = "") {
    const out = [];
    // Iterating the string (not indexing it) yields whole code points, so a
    // surrogate pair encodes as one 4-byte sequence rather than two broken ones.
    for (const ch of String(str)) {
      const c = ch.codePointAt(0);
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
}

class TextDecoder {
  #fatal;
  constructor(label = "utf-8", options = {}) {
    const l = String(label).toLowerCase();
    if (l !== "utf-8" && l !== "utf8" && l !== "unicode-1-1-utf-8") {
      throw new RangeError(`only utf-8 is supported, not ${label}`);
    }
    this.#fatal = !!options.fatal;
  }
  get encoding() {
    return "utf-8";
  }
  decode(input) {
    if (input == null) return "";
    const u8 =
      input instanceof Uint8Array
        ? input
        : input instanceof ArrayBuffer
          ? new Uint8Array(input)
          : ArrayBuffer.isView(input)
            ? new Uint8Array(input.buffer, input.byteOffset, input.byteLength)
            : null;
    if (!u8) throw new TypeError("decode expects bytes");

    let out = "";
    for (let i = 0; i < u8.length; ) {
      const b = u8[i];
      let n, cp;
      if (b < 0x80) { n = 1; cp = b; }
      else if ((b & 0xe0) === 0xc0) { n = 2; cp = b & 0x1f; }
      else if ((b & 0xf0) === 0xe0) { n = 3; cp = b & 0x0f; }
      else if ((b & 0xf8) === 0xf0) { n = 4; cp = b & 0x07; }
      else { n = 0; }

      if (n === 0 || i + n > u8.length) {
        if (this.#fatal) throw new TypeError("invalid UTF-8 in input");
        out += "\uFFFD";
        i++;
        continue;
      }
      let bad = false;
      for (let k = 1; k < n; k++) {
        const c = u8[i + k];
        if ((c & 0xc0) !== 0x80) { bad = true; break; }
        cp = (cp << 6) | (c & 0x3f);
      }
      if (bad) {
        if (this.#fatal) throw new TypeError("invalid UTF-8 in input");
        out += "\uFFFD";
        i++;
        continue;
      }
      out += String.fromCodePoint(cp);
      i += n;
    }
    return out;
  }
}
globalThis.TextEncoder = TextEncoder;
globalThis.TextDecoder = TextDecoder;

// structuredClone. A deep copy that handles cycles, which is the reason it exists
// at all — JSON.parse(JSON.stringify(x)) throws on a cycle, silently turns a Date
// into a string, and drops undefined.
globalThis.structuredClone = (value) => {
  const seen = new Map();
  const clone = (v) => {
    if (v === null || typeof v !== "object") {
      if (typeof v === "function" || typeof v === "symbol") {
        throw new TypeError(`${typeof v} could not be cloned`);
      }
      return v;
    }
    if (seen.has(v)) return seen.get(v);

    let out;
    if (v instanceof Date) out = new Date(v.getTime());
    else if (v instanceof RegExp) out = new RegExp(v.source, v.flags);
    else if (v instanceof ArrayBuffer) out = v.slice(0);
    else if (ArrayBuffer.isView(v)) {
      out = new v.constructor(clone(v.buffer), v.byteOffset, v.length ?? v.byteLength);
      seen.set(v, out);
      return out;
    } else if (Array.isArray(v)) {
      out = [];
      seen.set(v, out);
      for (const item of v) out.push(clone(item));
      return out;
    } else if (v instanceof Map) {
      out = new Map();
      seen.set(v, out);
      for (const [k, val] of v) out.set(clone(k), clone(val));
      return out;
    } else if (v instanceof Set) {
      out = new Set();
      seen.set(v, out);
      for (const item of v) out.add(clone(item));
      return out;
    } else if (v instanceof Error) {
      out = new v.constructor(v.message);
      seen.set(v, out);
      return out;
    } else {
      out = {};
      seen.set(v, out);
      for (const k of Object.keys(v)) out[k] = clone(v[k]);
      return out;
    }
    seen.set(v, out);
    return out;
  };
  return clone(value);
};

// URL and URLSearchParams. A pragmatic subset, not a WHATWG-conformant parser:
// it handles the shapes a tool actually builds and reads — absolute URLs, and
// resolution of a relative reference against a base — and does not implement
// IDNA, percent-encoding normalization of the host, or the full state machine.
// Documented as a subset rather than quietly approximated.
class URLSearchParams {
  #pairs = [];
  constructor(init = "") {
    if (init instanceof URLSearchParams) {
      this.#pairs = init.#pairs.map(([k, v]) => [k, v]);
    } else if (typeof init === "string") {
      for (const part of init.replace(/^\?/, "").split("&")) {
        if (!part) continue;
        const i = part.indexOf("=");
        const k = i < 0 ? part : part.slice(0, i);
        const v = i < 0 ? "" : part.slice(i + 1);
        this.#pairs.push([dec(k), dec(v)]);
      }
    } else if (Array.isArray(init)) {
      for (const [k, v] of init) this.#pairs.push([String(k), String(v)]);
    } else if (init && typeof init === "object") {
      for (const k of Object.keys(init)) this.#pairs.push([k, String(init[k])]);
    }
  }
  get(name) {
    const hit = this.#pairs.find(([k]) => k === name);
    return hit ? hit[1] : null;
  }
  getAll(name) {
    return this.#pairs.filter(([k]) => k === name).map(([, v]) => v);
  }
  has(name) {
    return this.#pairs.some(([k]) => k === name);
  }
  set(name, value) {
    const i = this.#pairs.findIndex(([k]) => k === name);
    if (i < 0) this.#pairs.push([name, String(value)]);
    else {
      this.#pairs[i] = [name, String(value)];
      this.#pairs = this.#pairs.filter(([k], j) => k !== name || j <= i);
    }
  }
  append(name, value) {
    this.#pairs.push([name, String(value)]);
  }
  delete(name) {
    this.#pairs = this.#pairs.filter(([k]) => k !== name);
  }
  sort() {
    this.#pairs.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  }
  keys() { return this.#pairs.map(([k]) => k)[Symbol.iterator](); }
  values() { return this.#pairs.map(([, v]) => v)[Symbol.iterator](); }
  entries() { return this.#pairs.map(([k, v]) => [k, v])[Symbol.iterator](); }
  [Symbol.iterator]() { return this.entries(); }
  forEach(fn, thisArg) {
    for (const [k, v] of this.#pairs) fn.call(thisArg, v, k, this);
  }
  get size() { return this.#pairs.length; }
  toString() {
    return this.#pairs
      .map(([k, v]) => `${enc(k)}=${enc(v)}`)
      .join("&");
  }
}

// application/x-www-form-urlencoded: space is "+", and the set of characters left
// alone is narrower than encodeURIComponent's.
function enc(s) {
  return encodeURIComponent(s).replace(/[!'()*]/g, (c) =>
    "%" + c.charCodeAt(0).toString(16).toUpperCase(),
  );
}
function dec(s) {
  try {
    return decodeURIComponent(s.replace(/\+/g, " "));
  } catch {
    return s; // a malformed escape is left as-is rather than throwing mid-parse
  }
}

const URL_RE = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/?#]*)([^?#]*)(\?[^#]*)?(#.*)?$/;

class URL {
  constructor(input, base) {
    let href = String(input);
    if (!URL_RE.test(href)) {
      if (base === undefined) throw new TypeError(`invalid URL: ${input}`);
      href = resolve(String(base), href);
    }
    const m = URL_RE.exec(href);
    if (!m) throw new TypeError(`invalid URL: ${input}`);

    const [, scheme, authority, path, query, hash] = m;
    this.protocol = scheme.toLowerCase() + ":";

    let auth = authority;
    const at = auth.lastIndexOf("@");
    if (at >= 0) {
      const cred = auth.slice(0, at);
      auth = auth.slice(at + 1);
      const ci = cred.indexOf(":");
      this.username = ci < 0 ? cred : cred.slice(0, ci);
      this.password = ci < 0 ? "" : cred.slice(ci + 1);
    } else {
      this.username = "";
      this.password = "";
    }
    const ci = auth.lastIndexOf(":");
    if (ci > auth.lastIndexOf("]")) {
      this.hostname = auth.slice(0, ci).toLowerCase();
      this.port = auth.slice(ci + 1);
    } else {
      this.hostname = auth.toLowerCase();
      this.port = "";
    }

    this.pathname = path || "/";
    this.hash = hash || "";
    this.searchParams = new URLSearchParams(query || "");
  }
  get host() {
    return this.port ? `${this.hostname}:${this.port}` : this.hostname;
  }
  get origin() {
    return `${this.protocol}//${this.host}`;
  }
  get search() {
    const q = this.searchParams.toString();
    return q ? "?" + q : "";
  }
  set search(v) {
    this.searchParams = new URLSearchParams(String(v));
  }
  get href() {
    const cred = this.username
      ? `${this.username}${this.password ? ":" + this.password : ""}@`
      : "";
    return `${this.protocol}//${cred}${this.host}${this.pathname}${this.search}${this.hash}`;
  }
  toString() {
    return this.href;
  }
  toJSON() {
    return this.href;
  }
}

// Relative resolution, covering the cases a tool hits: absolute path, query-only,
// fragment-only, and a relative segment against the base's directory.
function resolve(base, ref) {
  const m = URL_RE.exec(base);
  if (!m) throw new TypeError(`invalid base URL: ${base}`);
  const [, scheme, authority, path] = m;
  const root = `${scheme}://${authority}`;
  if (ref.startsWith("//")) return `${scheme}:${ref}`;
  if (ref.startsWith("/")) return root + ref;
  if (ref.startsWith("?") || ref.startsWith("#")) {
    return root + (path || "/") + ref;
  }
  const dir = (path || "/").replace(/[^/]*$/, "");
  const segs = (dir + ref).split("/");
  const out = [];
  for (const seg of segs) {
    if (seg === "." || seg === "") continue;
    if (seg === "..") out.pop();
    else out.push(seg);
  }
  const trailing = ref.endsWith("/") ? "/" : "";
  return `${root}/${out.join("/")}${trailing}`;
}
globalThis.URL = URL;
globalThis.URLSearchParams = URLSearchParams;

// ── locale formatting: fail loudly rather than quietly wrongly ────────────
// This build has no Intl — ICU is megabytes of tables against a 1 MB interpreter
// that is compiled into every nine binary, and that tradeoff is not close.
//
// What is not acceptable is the fallback's behavior. QuickJS's Intl-less
// toLocaleString *accepts* a locale and an options bag and ignores them:
//
//   new Date(0).toLocaleString("en-US", { timeZone: "Europe/Paris" })
//     => "01/01/1970, 12:00:00 AM"   — UTC. An hour off. No diagnostic.
//
// A timezone conversion built on that is wrong in a way that only shows up in
// production. So a call that passes arguments we cannot honor throws, and a call
// that passes none keeps working, because the default format is not a lie
// (docs/rich-js-tools.md §8.2, decided).
function guardLocale(proto, name) {
  const original = proto[name];
  proto[name] = function (...args) {
    if (args.length > 0 && args.some((a) => a !== undefined)) {
      throw new TypeError(
        `${name}(locale, options) needs Intl, which this runtime does not have — ` +
          `the arguments would be ignored and the result would be silently wrong. ` +
          `Format explicitly, or import "nine:date".`,
      );
    }
    return original.call(this);
  };
}
guardLocale(Date.prototype, "toLocaleString");
guardLocale(Date.prototype, "toLocaleDateString");
guardLocale(Date.prototype, "toLocaleTimeString");
guardLocale(Number.prototype, "toLocaleString");
guardLocale(BigInt.prototype, "toLocaleString");

// ── timers ────────────────────────────────────────────────────────────────
// A tool must never sleep: it runs inside one turn, under a wall-clock deadline
// that is also the only CPU bound there is. So these are *virtual time* — the
// queue runs in deadline order, and no real time passes (docs/rich-js-tools.md
// §8.1, decided).
//
// That makes ordering work, which is what bundled dependencies that debounce or
// back off actually depend on. It also means Date.now() will show no elapsed time
// across a setTimeout(fn, 1000). That is the surprising part, and it is
// deliberate: the alternative is burning a fifth of the call's deadline asleep.
const timers = new Map();
let timerSeq = 1;
let virtualNow = 0;
let timerBudget = 10000;

function schedule(fn, delay, args, repeat) {
  if (typeof fn !== "function") throw new TypeError("callback must be a function");
  const d = Number(delay) || 0;
  const id = timerSeq++;
  timers.set(id, { at: virtualNow + Math.max(0, d), fn, args, repeat: repeat ? Math.max(1, d) : 0, seq: id });
  return id;
}

globalThis.setTimeout = (fn, delay, ...args) => schedule(fn, delay, args, false);
globalThis.setInterval = (fn, delay, ...args) => schedule(fn, delay, args, true);
globalThis.clearTimeout = (id) => timers.delete(id);
globalThis.clearInterval = (id) => timers.delete(id);

// Run the earliest due timer. Ties break by creation order, matching the ordering
// a real event loop gives you.
function runNextTimer() {
  let next = null;
  for (const [, t] of timers) {
    if (!next || t.at < next.at || (t.at === next.at && t.seq < next.seq)) next = t;
  }
  if (!next) return false;
  if (--timerBudget <= 0) {
    throw new Error(
      "timer budget exhausted: a setInterval that never stops cannot run to completion in a sandboxed tool",
    );
  }
  virtualNow = next.at;
  if (next.repeat) next.at = virtualNow + next.repeat;
  else timers.delete(next.seq);
  next.fn(...next.args);
  return true;
}

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

  const res = JSON.parse(hostHTTP(JSON.stringify(req)));
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

// Run the tool to completion, driving the timer queue while it waits.
//
// The drain has to interleave with the tool's own promise rather than run after
// it: `await new Promise(r => setTimeout(r, 50))` is the whole point of having
// timers, and a queue drained only after settlement would deadlock on exactly
// that. So each pass yields to the microtask queue (`await null`, which
// qjs_host.c's job loop keeps turning), checks whether the tool finished, and
// otherwise advances virtual time by one timer.
async function run(fn) {
  let settled = false;
  let value, failure, failed = false;

  const p = (async () => fn(toolArgs))().then(
    (v) => { value = v; settled = true; },
    (e) => { failure = e; failed = true; settled = true; },
  );

  for (;;) {
    await null;
    if (settled) break;
    if (!runNextTimer()) break; // nothing left to advance; wait on the promise
  }
  if (!settled) await p; // no timers pending: settle or hit the wall clock
  if (failed) throw failure;
  return value;
}

try {
  // Dynamic, and *after* everything above is installed. A static import would be
  // hoisted above this module's body, so the tool's module-level code would run
  // in a world with no console, no fetch, and none of the platform layer.
  const mod = await import("nine:tool");
  const tool = mod.default;
  if (typeof tool !== "function") {
    throw new TypeError(
      "tool must default-export a function: export default (args) => ...",
    );
  }
  const value = await run(tool);
  globalThis.__nine_result = JSON.stringify({ ok: true, output: render(value) });
} catch (e) {
  globalThis.__nine_result = JSON.stringify({
    ok: false,
    error: e && e.message ? String(e.message) : String(e),
    error_detail: detail(e),
  });
}
