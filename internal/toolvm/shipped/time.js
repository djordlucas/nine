// time — the current date and time, in UTC.
//
// The first shipped sandboxed tool. It replaces the `time` built-in plugin,
// which was a subprocess running with the daemon's full uid authority in order
// to read a clock. This declares no capability at all: no filesystem pre-open,
// no host function beyond the ABI. That is the migration's point — a clock does
// not need the operator's home directory in reach.
//
// **This reports UTC, where the plugin reported the daemon's local time and
// zone.** That is a real behavior change and not an oversight. The guest has no
// timezone database: QuickJS ships no Intl, and `nine:date` is UTC-only by
// design. A tool cannot know the host's zone unless the host confers it, and the
// sandbox exists to withhold exactly that kind of ambient fact.
//
// Formatting is explicit for the same reason — toLocaleString would need Intl,
// and the harness rejects it rather than silently ignoring the locale and
// returning something subtly wrong.

const DAYS = [
  "Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday",
];
const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];

const pad = (n) => String(n).padStart(2, "0");

export default function () {
  const t = new Date();

  const hours24 = t.getUTCHours();
  const meridiem = hours24 < 12 ? "AM" : "PM";
  // 0 → 12, 13 → 1. Written out because getUTCHours() % 12 gives 0 at noon.
  const hours12 = hours24 % 12 === 0 ? 12 : hours24 % 12;

  const human =
    `${DAYS[t.getUTCDay()]}, ${MONTHS[t.getUTCMonth()]} ${t.getUTCDate()}, ` +
    `${t.getUTCFullYear()} ${hours12}:${pad(t.getUTCMinutes())}:` +
    `${pad(t.getUTCSeconds())} ${meridiem} UTC`;

  return {
    iso8601: t.toISOString(),
    human,
    timezone: "UTC",
  };
}
