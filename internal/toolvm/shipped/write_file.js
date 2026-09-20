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

import { stat, writeFile, appendFile, mkdir } from "nine:fs";

const ROOT = "/work";

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

export default function ({ path, content, mode, if_unchanged }) {
  const target = resolve(path);
  const body = String(content ?? "");
  const append = String(mode ?? "") === "append";

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

  writeFile(target, body);
  return `wrote ${target}`;
}
