// nine:fs — the filesystem, as far as the operator granted it.
//
// This closes the gap that motivated adr/rich-js-tools.md: `fs.read` and
// `fs.write` were declarable, grantable, validated at load, and printed by
// `nine tools`, and a `js` tool handed either had no API with which to use them.
//
// The paths here are the *guest* paths from the grant — `/data`, not whatever
// that is on the operator's disk — which is what lets them move or narrow the
// mount without your tool changing. Containment is wazero's: a tool scoped to
// one pre-open cannot walk out of it, and nothing in this file is what stops it.
// The capability checks below exist only so an ungranted call says so, instead of
// surfacing "no such file" for a file that plainly exists.

const I = globalThis[Symbol.for("nine.internal")];
const caps = I.caps();

function need(cap, what) {
  if (!cap || cap.length === 0) {
    throw new Error(
      `${what} is not granted to this tool — declare it in your manifest ` +
        `and have the operator grant it in nine.toml`,
    );
  }
}

/** Read a file as bytes. Use TextDecoder for text, or readFileText below. */
export function readFile(path) {
  // A write mount is readable too, so either grant admits a read.
  if (!caps.fs_read?.length && !caps.fs_write?.length) {
    need(null, "fs.read");
  }
  return I.fsRead(String(path));
}

/** Read a file and decode it as UTF-8. */
export function readFileText(path) {
  return new TextDecoder().decode(readFile(path));
}

/** Write bytes or a string. Requires fs.write. */
export function writeFile(path, data) {
  need(caps.fs_write, "fs.write");
  return I.fsWrite(String(path), data);
}

/** Create a directory and any missing parents. Requires fs.write.
 *
 * Recursive because that is the case that comes up: writing
 * "notes/2026/today.md" into an empty mount needs two directories made, and a
 * tool should not have to loop over path components it cannot see the root of.
 * An existing directory is success, so calling this before a write is cheap.
 */
export function mkdir(path) {
  need(caps.fs_write, "fs.write");
  return I.fsMkdir(String(path));
}

/** List a directory: names only, no recursion. */
export function readDir(path) {
  if (!caps.fs_read?.length && !caps.fs_write?.length) {
    need(null, "fs.read");
  }
  return I.fsReadDir(String(path));
}

/** { size, isFile, isDirectory, mtimeMs }, or null when the path does not exist. */
export function stat(path) {
  if (!caps.fs_read?.length && !caps.fs_write?.length) {
    need(null, "fs.read");
  }
  return I.fsStat(String(path));
}

/** True when the path exists. */
export function exists(path) {
  return stat(path) !== null;
}

/** Read `length` bytes starting at byte `offset`. Returns a Uint8Array, short
 * at end of file and empty past it.
 *
 * This is how a tool reads a file bigger than its own memory. readFile holds
 * the whole thing; a call is capped at 16 MiB by default, and workspaces hold
 * files larger than that. A paging loop over readRange holds one window.
 */
export function readRange(path, offset, length) {
  if (!caps.fs_read?.length && !caps.fs_write?.length) {
    need(null, "fs.read");
  }
  return I.fsReadRange(String(path), Number(offset) | 0, Number(length) | 0);
}

/** readRange, decoded as UTF-8. A window may split a multi-byte character at
 * either end; decode the bytes yourself when that matters. */
export function readRangeText(path, offset, length) {
  return new TextDecoder().decode(readRange(path, offset, length));
}

/** Append bytes or a string, creating the file when absent. Requires fs.write.
 *
 * Adding to a file without rewriting it: the alternative is reading the whole
 * file back and writing it again, which costs memory proportional to the file
 * and loses everything already there if it is interrupted.
 */
export function appendFile(path, data) {
  need(caps.fs_write, "fs.write");
  return I.fsAppend(String(path), data);
}

/** Rename, within the mount. Requires fs.write.
 *
 * One inode operation whatever the file's size, which is what makes three
 * things affordable: relocating a large file, replacing a file atomically
 * (write a temporary, rename over the target), and moving a file aside instead
 * of destroying it.
 */
export function rename(from, to) {
  need(caps.fs_write, "fs.write");
  return I.fsRename(String(from), String(to));
}

/** Remove one file, or one empty directory. Requires fs.write.
 *
 * Not recursive, deliberately: a tool that means to delete a tree walks it and
 * says so at every step, so nothing erases a directory by accident.
 */
export function remove(path) {
  need(caps.fs_write, "fs.write");
  return I.fsUnlink(String(path));
}

/** Copy a file, streaming through a fixed buffer. Requires fs.write.
 *
 * Neither side is ever fully resident, so copying is bounded by the buffer
 * rather than by the file.
 */
export function copyFile(from, to, chunkBytes = 1 << 20) {
  need(caps.fs_write, "fs.write");
  const src = String(from);
  const dst = String(to);
  const info = stat(src);
  if (info === null) throw new Error(`cannot copy ${src}: no such file`);

  // Truncate any existing destination before appending, so a copy over a longer
  // file cannot leave the old tail behind.
  I.fsWrite(dst, new Uint8Array(0));
  for (let off = 0; off < info.size; off += chunkBytes) {
    const chunk = I.fsReadRange(src, off, chunkBytes);
    if (chunk.length === 0) break;
    I.fsAppend(dst, chunk);
  }
  return info.size;
}

/**
 * The mounts this tool actually has, as guest paths:
 *
 *   { read: ["/data"], write: ["/out"] }
 *
 * Worth reading rather than assuming — the operator chose these, and a tool that
 * hardcodes a path the operator did not mount fails for a reason that looks like
 * a missing file.
 */
export function mounts() {
  return { read: caps.fs_read ?? [], write: caps.fs_write ?? [] };
}
