// pursue — the process behind every goal session and standing agent
// (adr/process-sessions.md §4). It is bound to its goal: Nine starts it while
// the goal is active and stops it otherwise, and hands it the goal with each
// clock tick. A tick asks the session to work on the goal; a message — a
// condition trigger's finding, piped to this session — is put to the session
// as it is.
//
// A recurring goal (one with a schedule) is never done: each tick is one of
// its runs, which does the whole job again. A goal without one is worked on
// until it is attained. Either way, what it produces goes in its folder.
import { next, turn } from "nine:process";

const folder = (g) =>
  g.folder
    ? ` Unless the goal names where its output goes, keep what it produces in ${g.folder}: ` +
      "look there first for what earlier turns wrote, and update those files rather than writing new copies elsewhere."
    : "";

const ongoing = (g) =>
  `Check on goal ${g.id} ("${g.description}") and its subtree (goal_get). ` +
  "Take any useful action toward it, including spawning sub-goals or sub-agents. " +
  "If its status should change (e.g. done once attained, or paused if you're stuck), " +
  "call goal_update_status." +
  folder(g);

const recurring = (g) =>
  `This is a scheduled run of recurring goal ${g.id} ("${g.description}"), which runs ${cadence(g.schedule)}. ` +
  "Do the whole job now: find what is new since the last run, then write or update its result. " +
  "When the result is written, end the turn. " +
  "A recurring goal is never done: leave its status active, unless it cannot go on (paused) " +
  "or should stop for good (archived)." +
  folder(g);

const cadence = (s) => (/^\d+[hms]$/.test(s) ? `every ${s}` : `on the cron schedule "${s}" (UTC)`);

export default () => {
  for (;;) {
    const t = next();
    if (t.kind === "clock" && t.goal) {
      turn(t.goal.schedule ? recurring(t.goal) : ongoing(t.goal));
    } else if (t.kind === "message" && t.text) {
      turn(t.text);
    }
  }
};
