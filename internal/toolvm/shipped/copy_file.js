// copy_file — duplicate a file inside the workspace.
//
// Streams through a fixed buffer, so the bytes never enter the model's context
// and neither side is ever fully resident. The alternative available before this
// tool was read_file followed by write_file, which spends the whole file's
// tokens twice and stops working past the context window.

import { stat, copyFile, mkdir } from "nine:fs";

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
  if (info.isDirectory) throw new Error(`${src} is a directory; copy_file copies one file`);

  const existing = stat(dst);
  if (existing !== null && !overwrite) {
    throw new Error(
      `${dst} already exists. Pass overwrite: true to replace it, or choose another path.`,
    );
  }

  const cut = dst.lastIndexOf("/");
  const parent = cut <= 0 ? "" : dst.slice(0, cut);
  if (parent && parent !== ROOT) mkdir(parent);

  const bytes = copyFile(src, dst);
  return `copied ${src} to ${dst} (${bytes} bytes)`;
}
