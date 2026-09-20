// restore_file — bring a trashed file back to the workspace.
//
// The other half of delete_file. An agent that notices its own mistake restores
// the file itself rather than reporting a loss to someone who may not be there
// for hours.
//
// A restore never clobbers: if something already occupies the destination, the
// call fails and says so. Recovering one file by destroying another is not a
// recovery.

import { stat, rename, mkdir, readDir } from "nine:fs";

const ROOT = "/work";
const TRASH = `${ROOT}/.nine/trash`;

function resolveTo(path) {
  const p = String(path ?? "").trim();
  if (p === "") return "";
  if (p === ROOT || p.startsWith(ROOT + "/")) return p;
  if (p.startsWith("/")) {
    throw new Error(
      `${p} is outside this tool's workspace. Paths resolve under ${ROOT} — ` +
        `use a relative path, or a ${ROOT}/... path.`,
    );
  }
  return `${ROOT}/${p}`;
}

// Every file an entry holds, with the workspace path it came from.
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
    out.push({ trashed: full, original: `${ROOT}/${rel}` });
  }
}

export default function ({ entry, path, to }) {
  const name = String(entry ?? "").trim();
  if (name === "") throw new Error("entry is required — list them with trash_list");
  if (name.includes("/")) {
    throw new Error(`entry must be a trash entry name, not a path (got ${name})`);
  }

  const dir = `${TRASH}/${name}`;
  if (stat(dir) === null) {
    throw new Error(`no trash entry named ${name}. It may have been swept; list what remains with trash_list.`);
  }

  const files = [];
  walk(dir, "", files);
  if (files.length === 0) throw new Error(`trash entry ${name} holds no files`);

  // An entry usually holds one file. `path` picks one when it holds several.
  const want = String(path ?? "").trim();
  const chosen = want ? files.filter((f) => f.original.includes(want)) : files;
  if (chosen.length === 0) {
    throw new Error(`trash entry ${name} has no file matching ${want}`);
  }
  if (chosen.length > 1) {
    throw new Error(
      `trash entry ${name} holds ${chosen.length} files; pass path to choose one of: ` +
        chosen.map((f) => f.original).join(", "),
    );
  }

  const file = chosen[0];
  const dest = resolveTo(to) || file.original;
  if (stat(dest) !== null) {
    throw new Error(
      `${dest} already exists. Restoring over it would destroy the current file — ` +
        `pass to with a free path if you want both.`,
    );
  }

  const cut = dest.lastIndexOf("/");
  const parent = cut <= 0 ? "" : dest.slice(0, cut);
  if (parent && parent !== ROOT) mkdir(parent);
  rename(file.trashed, dest);

  return `restored ${dest}${dest === file.original ? "" : ` (originally ${file.original})`}`;
}
