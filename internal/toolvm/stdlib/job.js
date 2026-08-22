// nine:job — how a long-running tool says "not done, ask me again".
//
// Your tool is still built from scratch for every call and thrown away after it.
// Nothing changes about that. What a resumable tool gets is the chance to hand
// back a *cursor* — your own resume point, opaque to Nine — which arrives as the
// second argument on the next call.
//
//   import { again } from "nine:job";
//
//   export default function (args, job) {
//     const at = Number(job?.cursor ?? 0);
//     const batch = process(at, at + 100);
//     if (at + 100 >= args.total) return `done: ${args.total} rows`;
//     return again({
//       cursor: String(at + 100),
//       progress: `${at + 100}/${args.total}`,
//       afterMs: 1000,
//     });
//   }
//
// Two rules that are not optional:
//
//   1. Do bounded work per call. Each one is an ordinary tool call under the
//      ordinary deadline — five seconds by default. Returning `again()` is how
//      you get more time; blocking is how you get killed.
//   2. Put everything you need to resume in the cursor, or in nine:state. The
//      instance does not survive, so a variable you set will not be there.
//
// Your manifest must say `resumable = true`. Without it the host refuses the
// envelope rather than silently running you forever.

const CONTINUE = Symbol.for("nine.continue");

/**
 * Ask to be called again.
 *
 * - `cursor`   — your resume point, handed back verbatim next call. A string.
 * - `progress` — one line the model and the operator see while you run.
 * - `afterMs`  — how long you would like before the next call. The host clamps
 *                it to at least its configured floor, so 0 does not mean "spin".
 */
export function again({ cursor = "", progress = "", afterMs = 0 } = {}) {
  return {
    [CONTINUE]: true,
    cursor: String(cursor),
    progress: String(progress),
    afterMs: Math.max(0, Math.trunc(Number(afterMs) || 0)),
  };
}

/** True if `v` came from again(). The harness uses this; tools rarely need it. */
export function isAgain(v) {
  return Boolean(v && typeof v === "object" && v[CONTINUE] === true);
}
