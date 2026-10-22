// reflect — the process behind self-reflection (adr/process-sessions.md §4):
// on each clock tick, ask the session to reflect and update its self-model. It
// runs as the self-reflection session's owner, or attached to a standing
// agent's session, where it reflects with that agent's history.
//
// Nine hands each tick a digest of what the journal recorded since the last
// reflection, and skips the tick when nothing happened.
import { next, turn } from "nine:process";

const prompt = (activity) =>
  (activity
    ? "Since your last reflection, the journal recorded:\n" + activity + "\n"
    : "Reflect on your recent sessions.\n") +
  "Use memory_set to update:\n" +
  "- `self/capabilities`: a concise description of what you can currently do\n" +
  "- `self/learned`: append a short dated entry with what this activity teaches about how you work: " +
  "what failed and why, what wasted effort or tokens, what worked. Skip what earlier entries already say.\n" +
  "Be brief and factual. Do not ask questions.";

export default () => {
  for (;;) {
    const t = next();
    if (t.kind === "clock") turn(prompt(t.activity));
  }
};
