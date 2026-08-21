// read_file — read a file from the workspace.
//
// Replaces half of the `files` built-in plugin, which was a subprocess holding
// the daemon's uid. That mattered most here: the plugin's read_file deliberately
// read **any absolute path**, its own comment saying "leaving other absolute
// paths as given so reads outside the workspace still work". Confinement was
// write-only.
//
// **This reads only what it is mounted.** A deliberate narrowing: a model that
// used to read /etc/anything now reads the workspace, and anything genuinely
// outside it becomes a mount the operator adds rather than an authority the tool
// always had. Containment is wazero's pre-open, not the check below — that
// exists so an out-of-mount path gets a sentence explaining itself rather than
// "no such file" for a file that plainly exists.
//
// `resolve` is duplicated in write_file.js rather than shared. A tool's source
// is served to the guest under one specifier, so a sibling import would mean
// inventing a module mechanism for twelve lines.

import { readFile } from "nine:fs";

const ROOT = "/work";

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

export default function ({ path }) {
  return new TextDecoder().decode(readFile(resolve(path)));
}
