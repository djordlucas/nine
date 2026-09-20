// trash_list — what is recoverable, newest first.
//
// Without this, the trash is write-only from the agent's side: delete_file moves
// a file somewhere it cannot enumerate, so an agent that deletes the wrong file
// cannot find its way back to it even though the bytes are still there. That
// would make the trash a benefit only to an operator watching at the time, which
// is exactly the case the trash exists to cover.
//
// .nine/ is otherwise excluded from every file tool. These two — trash_list and
// restore_file — are the only way in, and they reach nothing else under it.

import { stat, readDir } from "nine:fs";

const ROOT = "/work";
const TRASH = `${ROOT}/.nine/trash`;

// Entry names are <UTC>-<random>: 20260920T143015Z-1f2e3d4c.
function entryTime(name) {
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z-/.exec(name);
  if (!m) return null;
  return `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z`;
}

// walk lists the files an entry holds, as paths relative to the entry, so the
// result shows where each file came from in the workspace.
function walk(dir, prefix, out) {
  for (const name of readDir(dir)) {
    const full = `${dir}/${name}`;
    const rel = prefix ? `${prefix}/${name}` : name;
    const info = stat(full);
    if (info === null) continue;
    if (info.isDirectory) {
      walk(full, rel, out);
      continue;
    }
    out.push({ original_path: `${ROOT}/${rel}`, size: info.size });
  }
}

export default function ({ path, limit }) {
  const root = stat(TRASH);
  if (root === null) return JSON.stringify({ entries: [], note: "the trash is empty" });

  const filter = String(path ?? "").trim();
  const cap = Math.max(1, Number(limit ?? 50) | 0);

  const names = readDir(TRASH)
    .filter((n) => entryTime(n) !== null)
    .sort()
    .reverse(); // newest first: the mistake someone wants back is the recent one

  const entries = [];
  for (const name of names) {
    const files = [];
    walk(`${TRASH}/${name}`, "", files);
    const matched = filter
      ? files.filter((f) => f.original_path.includes(filter))
      : files;
    if (matched.length === 0) continue;
    entries.push({ entry: name, deleted_at: entryTime(name), files: matched });
    if (entries.length >= cap) break;
  }

  if (entries.length === 0) {
    return JSON.stringify({
      entries: [],
      note: filter
        ? `nothing in the trash matches ${filter}`
        : "the trash is empty",
    });
  }
  return JSON.stringify({ entries });
}
