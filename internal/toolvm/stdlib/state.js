// nine:state — what this tool is allowed to remember between calls.
//
// A tool is instantiated fresh for every call and torn down after it, so nothing
// in the interpreter survives: not a global, not a cached credential, not a
// parsed index. That is deliberate and unchanged. This module is the one way
// past it, and what it offers is not a hole in the sandbox but a store the
// *host* owns: keys scoped to your tool, bounded by a quota the operator set,
// and readable by nothing else.
//
// Scope is the operator's choice and you cannot change it:
//
//   scope = "tool"          one namespace shared by every caller of this tool
//   scope = "conversation"  a separate namespace per conversation
//
// Call caps() (from nine:caps, or nine.caps) if you need to know which you got —
// a cache is correct under either, but "remember this user's preference" means
// something different under each.
//
// Values are strings. Use JSON.stringify/parse for anything else.

const I = globalThis[Symbol.for("nine.internal")];

function call(req) {
  const res = JSON.parse(I.state(JSON.stringify(req)));
  if (res.error) {
    const err = new Error(res.error);
    // A quota refusal is a normal thing to hit and a normal thing to handle:
    // evict something and retry. It is deliberately distinguishable from the
    // store being unreachable, which is not your problem to recover from.
    err.code = res.quota ? "E_STATE_QUOTA" : "E_STATE";
    err.retryable = Boolean(res.quota);
    throw err;
  }
  return res;
}

/** Read a value. Returns undefined when the key is absent or has expired. */
export function get(key) {
  const res = call({ op: "get", key: String(key) });
  return res.found ? res.value : undefined;
}

/** Write a value, replacing any existing one. Throws if it would exceed quota. */
export function set(key, value) {
  call({ op: "set", key: String(key), value: String(value) });
}

/** Remove a key. Removing an absent key is not an error. */
export function remove(key) {
  call({ op: "delete", key: String(key) });
}

/** List this namespace's keys, optionally under a literal prefix. Sorted. */
export function keys(prefix = "") {
  return call({ op: "list", prefix: String(prefix) }).keys ?? [];
}

/**
 * Compare-and-set. Writes `value` only if the current value is exactly
 * `expected`, or — when `expected` is null or undefined — only if the key is
 * absent. Returns true if the write took.
 *
 * Reach for this whenever you would otherwise read, modify, and write back. Two
 * turns calling one tool at once is ordinary, so that sequence is a lost update
 * waiting to happen: both calls read the same value and the second overwrites
 * the first. This is the only operation that closes that window.
 *
 *   let ok = false;
 *   while (!ok) {
 *     const cur = get("count");
 *     ok = swap("count", cur, String(Number(cur ?? 0) + 1));
 *   }
 */
export function swap(key, expected, value) {
  const req = { op: "swap", key: String(key), value: String(value) };
  if (expected !== null && expected !== undefined) req.expected = String(expected);
  return call(req).ok === true;
}

/**
 * The scope the operator granted: "tool" or "conversation", or undefined if
 * state was not granted at all.
 *
 * You cannot change it — this is here so a tool whose behaviour depends on the
 * answer can read it rather than assume. A cache is correct under either scope;
 * "remember this user's preference" is not.
 */
export function scope() {
  return I.caps().state?.scope;
}

/** Read a JSON value, or undefined. Convenience over get + JSON.parse. */
export function getJSON(key) {
  const raw = get(key);
  return raw === undefined ? undefined : JSON.parse(raw);
}

/** Write a JSON value. Convenience over JSON.stringify + set. */
export function setJSON(key, value) {
  set(key, JSON.stringify(value));
}
