// reflect — the process behind self-reflection (adr/process-sessions.md §4):
// on each clock tick, ask the session to reflect and update its self-model. It
// runs as the self-reflection session's owner, or attached to a standing
// agent's session, where it reflects with that agent's history.
import { next, turn } from "nine:process";

const prompt =
  "You have been idle. Reflect on your recent sessions. Use memory_set to update:\n" +
  "- `self/capabilities`: a concise description of what you can currently do\n" +
  "- `self/learned`: append a short dated entry with key insights from recent activity\n" +
  "Be brief and factual. Do not ask questions.";

export default () => {
  for (;;) {
    if (next().kind === "clock") turn(prompt);
  }
};
