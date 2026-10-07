// pursue — the process behind every goal session and standing agent
// (adr/process-sessions.md §4). It is bound to its goal: Nine starts it while
// the goal is active and stops it otherwise, and hands it the goal with each
// clock tick. A tick asks the session to work on the goal; a message — a
// condition trigger's finding, piped to this session — is put to the session
// as it is.
import { next, turn } from "nine:process";

export default () => {
  for (;;) {
    const t = next();
    if (t.kind === "clock" && t.goal) {
      turn(
        `Check on goal ${t.goal.id} ("${t.goal.description}") and its subtree (goal_get). ` +
          "Take any useful action toward it, including spawning sub-goals or sub-agents. " +
          "If its status should change (e.g. done once attained, or paused if you're stuck), " +
          "call goal_update_status.",
      );
    } else if (t.kind === "message" && t.text) {
      turn(t.text);
    }
  }
};
