---
name: tool-authoring
description: Writing your own sandboxed tools with tool_write — the shape that works, what capabilities you can ask for, and how to make a tool remember between calls
tags: [tools, tool_write, sandboxed, javascript, state, capabilities]
---

## Writing Your Own Tools

`tool_write` gives you a real capability: you can add a tool to your own catalog and call
it on later turns. Use it when you find yourself doing the same mechanical work more than
once — parsing the same shape, reformatting the same report, walking the same tree.

A generated tool is JavaScript, running in a sandbox with **nothing** granted by default.
No filesystem, no network, no environment. A pure transform over its arguments is the
ideal shape and it is the one that always works.

```js
export default function ({ text }) {
  return text.split(/\s+/).filter(Boolean).length + " words";
}
```

The function's return value is what you see when you call the tool. Return a string.

### Ask for a capability only if you truly need it

Declare it, and the operator's ceiling decides whether you get it. Declaring something the
ceiling excludes is refused — you will get a message saying so, and the right response is
to rewrite without it or call `gap_report`, not to try a different phrasing.

| Need | Declare | Reach it with |
|---|---|---|
| read/write files | `fs: ["read"]`, `fs: ["write"]` | `import { readFileText, writeFile } from "nine:fs"` |
| an environment variable | `env: ["TZ"]` | `import { get } from "nine:env"` |
| fetch a URL | `net: ["http"]` | `fetch()` |
| remember between calls | `state: true` | `import { get, set } from "nine:state"` |

A tool that declares nothing gets nothing, whatever the ceiling permits. That is the
common case and the right one.

### Making a tool remember: `nine:state`

Your tool is built from scratch for every call and thrown away after it. Nothing survives —
not a global, not a cached lookup, not a parsed index. If you need it to remember, declare
`state: true` and use `nine:state`.

```js
import { get, set, remove, keys, swap, getJSON, setJSON, scope } from "nine:state";

export default function ({ place }) {
  const hit = getJSON(`geo:${place}`);
  if (hit) return `${hit.lat},${hit.lon} (cached)`;
  const fresh = lookUp(place);
  setJSON(`geo:${place}`, fresh);
  return `${fresh.lat},${fresh.lon}`;
}
```

Four things to get right:

**Values are strings.** Use `getJSON` / `setJSON` for anything else.

**Use `swap`, not read-then-write.** Two turns can call your tool at the same time, so
`get` followed by `set` loses updates: both calls read the same value and the second
overwrites the first. `swap(key, expected, value)` writes only if the current value is
still `expected`, and returns whether it took.

```js
let ok = false;
while (!ok) {
  const cur = get("count");
  ok = swap("count", cur, String(Number(cur ?? 0) + 1));
}
```

**Handle the quota.** Your store is bounded. Going over throws with `code` set to
`E_STATE_QUOTA` — a normal thing to recover from by evicting something, and deliberately
distinguishable from the store being broken.

```js
try {
  set(key, value);
} catch (e) {
  if (e.code !== "E_STATE_QUOTA") throw e;
  for (const k of keys("cache:").slice(0, 10)) remove(k);
  set(key, value);
}
```

**Know your scope.** The operator chose it and you cannot change it. `scope()` returns
`"tool"` (one namespace shared by every caller of this tool) or `"conversation"` (a
separate one per conversation). A cache is correct under either. Anything phrased as
"remember what *this* user said" is only correct under `"conversation"` — under `"tool"`
you would be showing one conversation's data to another. If it matters, check.

Do not put anything in state that you would not want a later call — possibly in a
different conversation — to read back.

### What to expect

- **A new tool is callable on your next turn**, not this one.
- **Your catalog is capped.** Writing past the cap evicts the least recently used tools.
  Prefer improving an existing tool to writing a near-duplicate — every tool competes for
  the same tool-selection budget, so a sprawling catalog makes you worse at choosing.
- **Use `js_eval` to iterate**, if it is enabled. It runs one snippet under identical rules
  and saves nothing, which is what keeps single-use experiments out of the catalog.
- **Write a real description.** It is how you will find the tool later.

### Checking your work

`tool_write` validates before it persists, so a refusal leaves nothing behind. Read the
refusal — it names the reason, and the reason is usually a capability the ceiling does not
allow or a name that collides with a built-in.
