// edit_file — replace exact text in a workspace file without rewriting it.
//
// The tool that was missing. write_file replaces a file whole, so changing one
// line of a 4 MB file meant reading 4 MB into the model's context and writing it
// back: impossible past the context window, lossy before it, and expensive
// always. Nothing else in the shipped set could change part of a file.
//
// The file is never resident. It is scanned in overlapping windows, the result
// is written to a temporary beside it, and the temporary is renamed over the
// original — so memory is bounded by the window rather than the file, and an
// interrupted edit leaves the original untouched.
//
// Matching is exact text, not a pattern: a model reproduces a line it has read
// far more reliably than it writes a regular expression, and an exact match has
// one obvious meaning when it fails.

import { stat, readRange, writeFile, appendFile, rename, remove, mkdir } from "nine:fs";

const ROOT = "/work";

const STATE = `${ROOT}/.nine`;
const TRASH = `${STATE}/trash`;

// refuseState keeps Nine's own bookkeeping out of reach of the file tools.
// trash_list and restore_file are the only way into .nine/, and they reach
// nothing else under it.
function refuseState(target) {
  if (target === STATE || target.startsWith(STATE + "/")) {
    throw new Error(
      `${target} is Nine's own bookkeeping and is not writable. ` +
        `Use trash_list and restore_file to reach a deleted file.`,
    );
  }
}

// The entry name the daemon's sweeper parses for age: <UTC>-<random>.
// Duplicated per tool for the reason resolve() is: a tool's source is served
// under one specifier, so there is no sibling to import.
function entryName() {
  const d = new Date();
  const p = (n) => String(n).padStart(2, "0");
  const stamp =
    `${d.getUTCFullYear()}${p(d.getUTCMonth() + 1)}${p(d.getUTCDate())}` +
    `T${p(d.getUTCHours())}${p(d.getUTCMinutes())}${p(d.getUTCSeconds())}Z`;
  const rand = Array.from(crypto.getRandomValues(new Uint8Array(4)))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
  return `${stamp}-${rand}`;
}

// trashTo moves a file aside instead of destroying it. Overwriting a file
// destroys its contents as thoroughly as deleting it does, and the agent doing
// the overwriting is frequently a session nobody is watching.
function trashTo(target) {
  const rel = target.slice(ROOT.length + 1);
  const dest = `${TRASH}/${entryName()}/${rel}`;
  mkdir(dest.slice(0, dest.lastIndexOf("/")));
  rename(target, dest);
  return dest;
}

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

// A window large enough that a match cannot straddle two windows undetected:
// the overlap carried between windows is the match length, so any occurrence is
// wholly inside some window.
const WINDOW = 1 << 20;

const enc = (s) => new TextEncoder().encode(s);

export default function ({ path, old_text, new_text, expect }) {
  const target = resolve(path);
  refuseState(target);

  const oldStr = String(old_text ?? "");
  if (oldStr === "") throw new Error("old_text is required and cannot be empty");
  const newStr = String(new_text ?? "");

  const info = stat(target);
  if (info === null) throw new Error(`no file at ${target}`);
  if (info.isDirectory) throw new Error(`${target} is a directory`);

  const oldBytes = enc(oldStr);
  const newBytes = enc(newStr);

  // "all" replaces every occurrence; a number demands exactly that many. The
  // default of 1 is the safe reading of an ambiguous instruction: a model that
  // meant every occurrence can say so, while one that assumed a single match
  // gets told the count rather than a file edited in places it never saw.
  const all = expect === "all";
  const want = all ? -1 : expect === undefined || expect === null ? 1 : Number(expect);
  if (!all && (!Number.isInteger(want) || want < 1)) {
    throw new Error(`expect must be a positive integer or "all", got ${JSON.stringify(expect)}`);
  }

  const hits = findAll(target, info.size, oldBytes, all ? Infinity : want + 1);
  if (hits.length === 0) {
    throw new Error(
      `old_text was not found in ${target}. It must match the file exactly, ` +
        `including indentation and line breaks — read the region first.`,
    );
  }
  if (!all && hits.length !== want) {
    throw new Error(
      `old_text occurs ${hits.length === want + 1 ? "more than " + want : hits.length} time(s) in ${target}, ` +
        `but expect is ${want}. Include more surrounding text to make it unique, ` +
        `or pass expect to match the count you intend.`,
    );
  }
  const tmp = `${target}.nine-edit-${Date.now().toString(36)}`;
  try {
    writeFile(tmp, new Uint8Array(0));
    let cursor = 0;
    for (const at of hits) {
      copyRange(target, tmp, cursor, at - cursor);
      if (newBytes.length) appendFile(tmp, newBytes);
      cursor = at + oldBytes.length;
    }
    copyRange(target, tmp, cursor, info.size - cursor);
    // The original is moved aside rather than replaced, so the version before
    // the edit is recoverable. Two renames, no copy: an edit to a 200 MB file
    // costs the same as an edit to a small one.
    trashTo(target);
    rename(tmp, target);
  } catch (e) {
    // A failed edit must not leave debris beside the file it did not change.
    try {
      remove(tmp);
    } catch {
      /* the rename may already have consumed it */
    }
    throw e;
  }

  const lines = countLines(target, hits[0]);
  return (
    `replaced ${hits.length} occurrence(s) in ${target}` +
    `; first at line ${lines}` +
    `, file is now ${stat(target).size} bytes`
  );
}

// findAll returns the byte offsets of every occurrence, stopping once `limit`
// have been found. Windows overlap by the needle length so a match spanning a
// boundary is still seen whole.
function findAll(path, size, needle, limit) {
  const out = [];
  const overlap = needle.length - 1;
  let base = 0;

  while (base < size && out.length < limit) {
    const chunk = readRange(path, base, WINDOW + overlap);
    if (chunk.length === 0) break;
    let from = 0;
    for (;;) {
      const at = indexOfBytes(chunk, needle, from);
      if (at < 0) break;
      const abs = base + at;
      // A window overlaps the previous one, so the same occurrence can appear
      // twice; the offsets are ascending, which makes the duplicate adjacent.
      if (out.length === 0 || out[out.length - 1] !== abs) out.push(abs);
      if (out.length >= limit) return out;
      from = at + 1;
    }
    base += WINDOW;
  }
  return out;
}

// indexOfBytes finds `needle` by letting the engine hunt for its first byte —
// TypedArray.indexOf is native, where a JS loop per byte is not. A 20 MB file
// scanned a byte at a time does not finish inside the 5-second call deadline
// (R-TVM.4), which is the difference between this tool working on the files it
// exists for and timing out on them.
function indexOfBytes(hay, needle, from) {
  const first = needle[0];
  const last = hay.length - needle.length;
  let i = from;
  while (i <= last) {
    const at = hay.indexOf(first, i);
    if (at < 0 || at > last) return -1;
    let j = 1;
    while (j < needle.length && hay[at + j] === needle[j]) j++;
    if (j === needle.length) return at;
    i = at + 1;
  }
  return -1;
}

// copyRange appends `length` bytes of `from` starting at `at` onto `to`, a
// window at a time, so neither file is ever fully resident.
function copyRange(from, to, at, length) {
  let done = 0;
  while (done < length) {
    const chunk = readRange(from, at + done, Math.min(WINDOW, length - done));
    if (chunk.length === 0) break;
    appendFile(to, chunk);
    done += chunk.length;
  }
}

// countLines reports the 1-based line the given byte offset falls on, read in
// windows like everything else here.
function countLines(path, offset) {
  let line = 1;
  for (let at = 0; at < offset; at += WINDOW) {
    const chunk = readRange(path, at, Math.min(WINDOW, offset - at));
    if (chunk.length === 0) break;
    for (let at = chunk.indexOf(0x0a); at !== -1; at = chunk.indexOf(0x0a, at + 1)) line++;
  }
  return line;
}
