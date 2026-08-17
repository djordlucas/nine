// nine:env — the environment variables the operator named for this tool.
//
// Scoping is not enforced here and does not need to be: the host passes only the
// granted keys into the instance, so a key nobody granted is not merely hidden,
// it is absent. The check below turns "undefined, for reasons you cannot see"
// into a sentence naming what was granted.
//
// `NINE_*` and `*_API_KEY` can never be granted at all — the daemon's own
// environment holds the LLM provider credentials.

const I = globalThis[Symbol.for("nine.internal")];
const caps = I.caps();
const granted = caps.env ?? [];

/** The value of a granted key, or undefined when it is unset in the daemon. */
export function get(name) {
  const key = String(name);
  if (granted.length === 0) {
    throw new Error(
      "env is not granted to this tool — declare env = [\"KEY\"] in your " +
        "manifest and have the operator grant it in nine.toml",
    );
  }
  if (!granted.includes(key)) {
    throw new Error(
      `env key ${key} is not granted to this tool; granted: ${granted.join(", ")}`,
    );
  }
  return I.env(key);
}

/** The keys this tool may read. */
export function keys() {
  return granted.slice();
}

/** Every granted key that is actually set, as an object. */
export function all() {
  const out = {};
  for (const k of granted) {
    const v = I.env(k);
    if (v !== undefined) out[k] = v;
  }
  return out;
}
