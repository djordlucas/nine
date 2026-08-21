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

import { writeFile, mkdir } from "nine:fs";

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

export default function ({ path, content }) {
  const target = resolve(path);
  const cut = target.lastIndexOf("/");
  const parent = cut <= 0 ? "" : target.slice(0, cut);
  // Idempotent and cheap: an existing directory is success.
  if (parent && parent !== ROOT) mkdir(parent);
  writeFile(target, String(content ?? ""));
  return `wrote ${target}`;
}
