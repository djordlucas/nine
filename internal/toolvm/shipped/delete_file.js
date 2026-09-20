// delete_file — remove a file from the workspace, recoverably.
//
// The file is moved into .nine/trash/ rather than destroyed. Approval gates arm
// only for interactive sessions (spec/contracts/hitl.md R-HITL.5), so a goal
// session, a standing agent or a sub-agent deletes with nobody watching; a trash
// makes that mistake recoverable instead of merely regrettable. The daemon
// bounds the trash by age and size, and restore_file brings a file back.
//
// Moving costs one rename whatever the file's size, because the trash is inside
// the same mount. A trash outside it would mean copying every deleted byte.
//
// Not recursive: one file, or one empty directory. The shell guard refuses
// `rm -r` for the same reason — a recursive delete is the one that cannot be
// partially wrong.

import { stat, rename, mkdir, readDir } from "nine:fs";

const ROOT = "/work";
const STATE = `${ROOT}/.nine`;
const TRASH = `${STATE}/trash`;

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

// The entry name the daemon's sweeper parses for age: <UTC>-<random>.
function entryName() {
  const d = new Date();
  const p = (n, w = 2) => String(n).padStart(w, "0");
  const stamp =
    `${d.getUTCFullYear()}${p(d.getUTCMonth() + 1)}${p(d.getUTCDate())}` +
    `T${p(d.getUTCHours())}${p(d.getUTCMinutes())}${p(d.getUTCSeconds())}Z`;
  const rand = Array.from(crypto.getRandomValues(new Uint8Array(4)))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
  return `${stamp}-${rand}`;
}

// trashTo moves a workspace path into a fresh trash entry, keeping its relative
// path so an operator reading the trash can see where a file came from.
function trashTo(target) {
  const rel = target.slice(ROOT.length + 1);
  const dir = `${TRASH}/${entryName()}`;
  const dest = `${dir}/${rel}`;
  const cut = dest.lastIndexOf("/");
  mkdir(dest.slice(0, cut));
  rename(target, dest);
  return dest;
}

export default function ({ path }) {
  const target = resolve(path);

  if (target === ROOT) throw new Error("refusing to delete the workspace root");
  if (target === STATE || target.startsWith(STATE + "/")) {
    throw new Error(
      `${target} is Nine's own bookkeeping (the trash lives there). Use restore_file to recover a deleted file.`,
    );
  }
  // A .git component is the operator's version control, not the agent's files.
  if (target.split("/").includes(".git")) {
    throw new Error(`refusing to delete ${target}: it is inside a git directory`);
  }

  const info = stat(target);
  if (info === null) throw new Error(`no file at ${target}`);
  if (info.isDirectory && readDir(target).length > 0) {
    throw new Error(
      `${target} is a directory and is not empty. Delete its contents first — ` +
        `there is no recursive delete.`,
    );
  }

  trashTo(target);
  return (
    `deleted ${target}` +
    (info.isDirectory ? " (empty directory)" : ` (${info.size} bytes)`) +
    `. It is recoverable with trash_list and restore_file until the trash is swept.`
  );
}
