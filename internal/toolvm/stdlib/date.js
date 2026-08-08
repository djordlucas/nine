// nine:date — small UTC date helpers for generated tools
// (docs/sandboxed-tools.md §4.2). Deliberately narrow: parsing, ISO-8601 week
// arithmetic, and a canonical YYYY-MM-DD render. No timezone database, no
// locale formatting — those want a real library, which the deps pipeline (§4.4)
// can provide when an operator enables it.

// parseDate(s) -> Date. Throws on an unparseable input rather than returning an
// Invalid Date that silently poisons everything downstream.
export function parseDate(s) {
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) throw new Error("invalid date: " + String(s));
  return d;
}

// isoWeek(date) -> 1..53, the ISO-8601 week number (weeks start Monday; week 1
// is the week containing the first Thursday of the year).
export function isoWeek(date) {
  const d = thursdayOf(date);
  const firstThursday = thursdayOf(new Date(Date.UTC(d.getUTCFullYear(), 0, 4)));
  return 1 + Math.round((d - firstThursday) / (7 * 24 * 3600 * 1000));
}

// isoWeekYear(date) -> the year that owns the ISO week (which can differ from
// the calendar year in late December / early January).
export function isoWeekYear(date) {
  return thursdayOf(date).getUTCFullYear();
}

// formatISODate(date) -> "YYYY-MM-DD" in UTC.
export function formatISODate(date) {
  const p = (n, w = 2) => String(n).padStart(w, "0");
  return `${p(date.getUTCFullYear(), 4)}-${p(date.getUTCMonth() + 1)}-${p(date.getUTCDate())}`;
}

// thursdayOf returns the UTC-midnight Thursday of the ISO week containing date;
// both isoWeek and isoWeekYear key off it, so the Monday-based shift lives once.
function thursdayOf(date) {
  const d = new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate()));
  const mondayBased = (d.getUTCDay() + 6) % 7; // Mon=0 … Sun=6
  d.setUTCDate(d.getUTCDate() - mondayBased + 3);
  return d;
}

export default { parseDate, isoWeek, isoWeekYear, formatISODate };
