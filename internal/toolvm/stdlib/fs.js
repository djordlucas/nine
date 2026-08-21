// nine:fs — the filesystem, as far as the operator granted it.
//
// This closes the gap that motivated docs/rich-js-tools.md: `fs.read` and
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
