// diff_file — what changed, as a diff a person can read.
//
// The "before" side costs nothing to keep: write_file, edit_file and
// delete_file already move the version they replace into .nine/trash/, so the
// previous contents of every changed file are on disk with a timestamp. This
// tool pairs the newest trashed version of a path with what is there now.
//
// It is how an agent answers "show me exactly what you changed" without being
// believed on its word, and how a person reviews an edit made in a session they
// were not watching.

import { stat, readDir, readRange } from "nine:fs";
import { hunks } from "nine:diff";

const ROOT = "/work";
const TRASH = `${ROOT}/.nine/trash`;

// Per-side ceiling on what is diffed. Above it the counts are still reported —
// a line-level LCS is exact and quadratic, and a person reading a chat message
// cannot use a diff of a 50 MB file anyway.
const MAX_SIDE = 1 << 20;

// Duplicated from read_file.js — see the note there.
function resolve(path) {
  const p = String(path ?? "").trim();
  if (p === "") throw new Error("path is required");
  if (p === ROOT || p.startsWith(ROOT + "/")) return p;
  if (p.startsWith("/")) {
    throw new Error(
      `${p} is outside this tool's workspace. Paths resolve under ${ROOT} — ` +
        `use a relative path, or a ${ROOT}/... path.`,
    );
  }
  return `${ROOT}/${p}`;
}

function entryTime(name) {
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z-/.exec(name);
  if (!m) return null;
  return `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z`;
}

function readWhole(target, size) {
  const take = Math.min(size, MAX_SIDE);
  const WINDOW = 1 << 18;
  let out = "";
  for (let at = 0; at < take; at += WINDOW) {
    out += new TextDecoder().decode(readRange(target, at, Math.min(WINDOW, take - at)));
  }
  return out;
}

// The trashed copy of `rel` inside one entry, if that entry holds it.
function trashedCopy(entry, rel) {
  const candidate = `${TRASH}/${entry}/${rel}`;
  const info = stat(candidate);
  return info === null || info.isDirectory ? null : { path: candidate, size: info.size };
}

export default function ({ path, against }) {
  const target = resolve(path);
  const rel = target.slice(ROOT.length + 1);

  const current = stat(target);
  const currentText = current === null ? "" : readWhole(target, current.size);

  let entry = String(against ?? "").trim();
  if (entry.startsWith("trash:")) entry = entry.slice("trash:".length);

  let previous = null;
  if (entry) {
    previous = trashedCopy(entry, rel);
    if (previous === null) {
      throw new Error(`trash entry ${entry} does not hold ${target}`);
    }
  } else {
    if (stat(TRASH) === null) throw new Error(`nothing is in the trash, so ${target} has no previous version to compare`);
    // Newest first: the version this file had before the most recent change.
    const names = readDir(TRASH)
      .filter((n) => entryTime(n) !== null)
      .sort()
      .reverse();
    for (const name of names) {
      const hit = trashedCopy(name, rel);
      if (hit !== null) {
        entry = name;
        previous = hit;
        break;
      }
    }
    if (previous === null) {
      throw new Error(
        `no previous version of ${target} is in the trash. ` +
          `Only a file that write_file, edit_file or delete_file has changed has one.`,
      );
    }
  }

  const previousText = readWhole(previous.path, previous.size);
  const h = hunks(previousText, currentText);

  return JSON.stringify({
    path: target,
    against: entry,
    previous_at: entryTime(entry),
    deleted: current === null,
    added_lines: h.added,
    removed_lines: h.removed,
    diff: h.text,
    truncated: h.truncated || previous.size > MAX_SIDE || (current?.size ?? 0) > MAX_SIDE,
    note:
      h.added === 0 && h.removed === 0
        ? "No line-level difference between the two versions."
        : undefined,
  });
}
