// move_file — relocate a file inside the workspace.
//
// A rename, not a copy: one inode operation whatever the file's size. Without
// it, moving a file meant read + write + delete, which costs the whole file in
// memory and is impossible for a file larger than a call's 16 MiB cap.
//
// The destination is refused when it exists, unless overwrite is set. A move
// that silently replaced a file would destroy one the model never looked at.

import { stat, rename, mkdir } from "nine:fs";

const ROOT = "/work";

// Duplicated from read_file.js — see the note there.
function resolve(path, label) {
  const p = String(path ?? "").trim();
  if (p === "") throw new Error(`${label} is required`);
  if (p === ROOT || p.startsWith(ROOT + "/")) return p;
  if (p.startsWith("/")) {
    throw new Error(
      `${p} is outside this tool's workspace. Paths resolve under ${ROOT} — ` +
        `use a relative path, or a ${ROOT}/... path.`,
    );
  }
  return `${ROOT}/${p}`;
}

export default function ({ from, to, overwrite }) {
  const src = resolve(from, "from");
  const dst = resolve(to, "to");
  if (src === dst) throw new Error(`from and to are the same path (${src})`);

  const info = stat(src);
  if (info === null) throw new Error(`no file at ${src}`);

  const existing = stat(dst);
  if (existing !== null && !overwrite) {
    throw new Error(
      `${dst} already exists. Pass overwrite: true to replace it, or choose another path.`,
    );
  }
  if (existing !== null && existing.isDirectory) {
    throw new Error(`${dst} is a directory; name the destination file itself`);
  }

  const cut = dst.lastIndexOf("/");
  const parent = cut <= 0 ? "" : dst.slice(0, cut);
  if (parent && parent !== ROOT) mkdir(parent);

  rename(src, dst);
  return `moved ${src} to ${dst}${existing !== null ? " (replaced)" : ""}`;
}
