// write_file — write a file into the workspace, creating parents as needed.
//
// Replaces half of the `files` built-in plugin. The plugin was already confined
// for writes; what it had and a sandboxed tool did not was the ability to create
// parent directories, which its description promises. That gap is why this
// migration waited for `mkdir` in the wasm host — a write_file that cannot make
// a directory is a downgrade for the software-dev and sysadmin roles, both of
// which carry this tool.
//
// Containment is wazero's pre-open: mkdir walks components inside the mount and
// cannot escape it, because the guest has nothing else to resolve against.

import { stat, writeFile, appendFile, mkdir, rename, readRange } from "nine:fs";
import { hunks } from "nine:diff";

const ROOT = "/work";

const STATE = `${ROOT}/.nine`;
const TRASH = `${STATE}/trash`;

// refuseState keeps Nine's own bookkeeping out of reach of the file tools.
// trash_list and restore_file are the only way into .nine/, and they reach
// nothing else under it.
// refuseSpill keeps the reserved prefix out of the workspace. read_file routes
// a spill/... path to the store, so a workspace file written there would be
// addressable by no tool at all — and a path that *looks* like a spill is
// exactly what a model would later cite as tool output.
function refuseSpill(target) {
  const rel = target.slice(ROOT.length + 1);
  if (rel === "spill" || rel.startsWith("spill/")) {
    throw new Error(
      `${target} is reserved: spill/ holds truncated tool output, which only Nine writes. ` +
        `Choose another path.`,
    );
  }
}

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

export default function ({ path, content, mode, if_unchanged, preview, content_ref }) {
  const target = resolve(path);
  // content_ref arrives already expanded: the daemon replaced the path the
  // model supplied with the content stored there (internal/agent/refs.go), so
  // by the time it reaches here it is the payload, not a reference.
  const body = String(content ?? content_ref ?? "");
  const append = String(mode ?? "") === "append";

  refuseState(target);
  refuseSpill(target);

  const info = stat(target);
  if (info !== null && info.isDirectory) throw new Error(`${target} is a directory`);

  // if_unchanged carries the version read_file returned. Two sub-agents writing
  // one file otherwise both succeed and the later one silently wins; with the
  // token, the loser is told what happened while its copy is still recoverable.
  if (if_unchanged !== undefined && if_unchanged !== null && String(if_unchanged) !== "") {
    const now = info === null ? "absent" : `${Math.round(info.mtimeMs)}-${info.size}`;
    if (now !== String(if_unchanged)) {
      throw new Error(
        `${target} changed since you read it (version ${now}, you passed ${if_unchanged}). ` +
          `Read it again and redo the change against the current contents.`,
      );
    }
  }

  // preview shows what the write would change, and changes nothing.
  if (preview) {
    const before = info === null ? "" : readWhole(target, info.size);
    const h = hunks(before, append ? before + body : body);
    return JSON.stringify({
      preview: true,
      path: target,
      exists: info !== null,
      added_lines: h.added,
      removed_lines: h.removed,
      diff: h.text,
      truncated: h.truncated,
      note: "Nothing was written. Call again without preview to apply this.",
    });
  }

  const cut = target.lastIndexOf("/");
  const parent = cut <= 0 ? "" : target.slice(0, cut);
  // Idempotent and cheap: an existing directory is success.
  if (parent && parent !== ROOT) mkdir(parent);

  // Appending is not a whole-file rewrite with extra steps: adding a line to a
  // log otherwise costs the file's size in memory and loses it entirely if the
  // write is cut short.
  if (append) {
    appendFile(target, body);
    return `appended ${body.length} character(s) to ${target}`;
  }

  // The previous contents go to the trash first, so an overwrite is as
  // recoverable as a delete. Identical content is not trashed: rewriting a file
  // with what it already holds is common, and a copy per rewrite would fill the
  // trash with duplicates of a file that never changed.
  let trashed = false;
  if (info !== null && !sameContent(target, info.size, body)) {
    trashTo(target);
    trashed = true;
  }

  writeFile(target, body);
  return `wrote ${target}${trashed ? " (previous version is in the trash)" : ""}`;
}

// sameContent reports whether the file already holds exactly `body`, comparing
// in windows so a large file is never resident.
function sameContent(target, size, body) {
  const bytes = new TextEncoder().encode(body);
  if (bytes.length !== size) return false;
  const WINDOW = 1 << 20;
  for (let at = 0; at < size; at += WINDOW) {
    const chunk = readRange(target, at, WINDOW);
    if (chunk.length === 0) return false;
    for (let i = 0; i < chunk.length; i++) {
      if (chunk[i] !== bytes[at + i]) return false;
    }
  }
  return true;
}

// readWhole reads a file in windows. A preview of a file too large to diff
// usefully is bounded here rather than by failing: the caller gets the head,
// and the counts still describe what would happen to it.
function readWhole(target, size) {
  const MAX = 1 << 20;
  const take = Math.min(size, MAX);
  let out = "";
  const WINDOW = 1 << 18;
  for (let at = 0; at < take; at += WINDOW) {
    out += new TextDecoder().decode(readRange(target, at, Math.min(WINDOW, take - at)));
  }
  return out;
}
