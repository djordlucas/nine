// nine:process — how a live process drives its session.
//
// A live process is a tool started by Nine to run until it is stopped. It waits
// for work with next(), asks the model with turn(), and hands results on with
// report(). None of this works in an ordinary call: a tool the model invoked
// gets an error from every function here, so it can never block a turn.
//
//   import { next, turn, report } from "nine:process";
//
//   export default (args) => {
//     for (;;) {
//       const trigger = next();            // blocks until the next trigger
//       const reply = turn(`Summarize: ${trigger.text ?? ""}`);
//       report(reply);
//     }
//   };
//
// When the process is stopped, the pending next() or turn() throws an error
// with code "E_STOPPED". Let it propagate: the process is over.

const I = globalThis[Symbol.for("nine.internal")];

function call(req) {
  const res = JSON.parse(I.process(JSON.stringify(req)));
  if (res.error) {
    const err = new Error(res.error);
    err.code = res.code || "E_PROCESS";
    err.retryable = Boolean(res.retryable);
    throw err;
  }
  return res;
}

/**
 * Wait for the next trigger and return it: { kind, at, text?, event?, from? }.
 * kind is "clock", "event" or "message". Blocks for as long as it takes.
 */
export function next() {
  return call({ op: "next" }).trigger;
}

/**
 * Run one model turn in this process's session and return the reply text.
 * Throws when the process's budget is spent (code "E_BUDGET") or the turn fails.
 */
export function turn(text) {
  return call({ op: "turn", text: String(text) }).reply;
}

/**
 * Report text to the session this process pipes to (report_to). Empty text is
 * ignored. Returns true when it was delivered.
 */
export function report(text) {
  const s = String(text ?? "");
  if (s === "") return false;
  return call({ op: "report", text: s }).delivered === true;
}
