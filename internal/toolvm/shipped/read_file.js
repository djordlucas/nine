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

import { stat, readFile, readRange } from "nine:fs";

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

// How much of a file is inspected to decide it is not text. A NUL byte is the
// cheap, reliable signal: valid UTF-8 text does not contain one.
const SNIFF = 8192;

// A window big enough to serve a line range in one read in the common case.
const WINDOW = 1 << 20;

const decodeStrict = (bytes) => new TextDecoder("utf-8", { fatal: true }).decode(bytes);

export default function ({ path, offset, limit, lines, line_numbers }) {
  const target = resolve(path);

  const info = stat(target);
  if (info === null) throw new Error(`no file at ${target}`);
  if (info.isDirectory) throw new Error(`${target} is a directory`);

  refuseBinary(target, info);

  // A version token identifies the file as it was read. Handing it back to a
  // write as if_unchanged turns a lost update into a reported conflict.
  const version = `${Math.round(info.mtimeMs)}-${info.size}`;

  if (lines !== undefined && lines !== null && String(lines).trim() !== "") {
    return JSON.stringify(readLines(target, info, String(lines), version, line_numbers));
  }

  const off = Math.max(0, Number(offset ?? 0) | 0);
  const lim = Math.max(0, Number(limit ?? 0) | 0);

  // A plain whole-file read stays a plain string: the common call keeps the
  // shape every caller already handles. Only a windowed read reports its
  // position, which is what a model needs to page onwards.
  if (off === 0 && lim === 0) {
    const text = decodeOrExplain(readFile(target), target);
    return line_numbers ? numberFrom(text, 1) : text;
  }

  const bytes = readRange(target, off, lim === 0 ? info.size - off : lim);
  const text = decodeOrExplain(trimPartialRunes(bytes, off > 0), target);
  return JSON.stringify({
    content: line_numbers ? numberFrom(text, 1) : text,
    offset: off,
    bytes: bytes.length,
    total: info.size,
    version,
  });
}

// refuseBinary stops a file that is not text from being decoded into
// replacement characters. A model cannot tell a U+FFFD it invented from one the
// file contained, so returning mojibake invites it to treat noise as content.
function refuseBinary(target, info) {
  const head = readRange(target, 0, Math.min(SNIFF, info.size));
  for (const b of head) {
    if (b === 0) {
      throw new Error(
        `${target} is not a text file (${info.size} bytes, contains NUL). ` +
          `read_file returns text; use a tool that understands the format.`,
      );
    }
  }
}

function decodeOrExplain(bytes, target) {
  try {
    return decodeStrict(bytes);
  } catch {
    throw new Error(
      `${target} is not valid UTF-8, so it cannot be returned as text. ` +
        `Read a byte range with offset and limit if you need part of it.`,
    );
  }
}

// trimPartialRunes drops a continuation-byte run at the start of a window and an
// incomplete sequence at its end, so a window landing mid-character decodes
// cleanly instead of failing.
function trimPartialRunes(bytes, trimHead) {
  let from = 0;
  let to = bytes.length;
  if (trimHead) {
    while (from < to && (bytes[from] & 0xc0) === 0x80) from++;
  }
  let back = to - 1;
  let seen = 0;
  while (back >= from && (bytes[back] & 0xc0) === 0x80 && seen < 3) {
    back--;
    seen++;
  }
  if (back >= from) {
    const lead = bytes[back];
    const need = lead >= 0xf0 ? 4 : lead >= 0xe0 ? 3 : lead >= 0xc0 ? 2 : 1;
    if (need > 1 && seen < need - 1) to = back;
  }
  return bytes.subarray(from, to);
}

// readLines serves a 1-based inclusive line range ("120-180", or "120" for one
// line). Models reason in lines and edit_file reports them, so a byte window is
// the wrong unit for "show me the part I am about to change".
function readLines(target, info, spec, version, numbered) {
  const m = /^(\d+)(?:\s*-\s*(\d+))?$/.exec(spec.trim());
  if (!m) throw new Error(`lines must look like "120-180" or "120", got ${JSON.stringify(spec)}`);
  const first = Number(m[1]);
  const last = m[2] === undefined ? first : Number(m[2]);
  if (first < 1) throw new Error("lines are 1-based; the first line is 1");
  if (last < first) throw new Error(`lines range ends before it starts (${spec})`);

  let line = 1;
  let carry = "";
  const picked = [];

  for (let at = 0; at < info.size && line <= last; at += WINDOW) {
    const chunk = readRange(target, at, WINDOW);
    if (chunk.length === 0) break;
    carry += decodeOrExplain(trimPartialRunes(chunk, false), target);
    const parts = carry.split("\n");
    carry = parts.pop() ?? "";
    for (const p of parts) {
      if (line >= first && line <= last) picked.push(p);
      line++;
      if (line > last) break;
    }
  }
  // The last line of a file without a trailing newline is whatever is left over.
  if (line <= last && carry !== "") {
    if (line >= first) picked.push(carry);
    line++;
  }

  if (picked.length === 0) {
    throw new Error(`${target} has no lines in range ${spec} (it has ${line - 1} line(s))`);
  }

  return {
    content: numbered ? numberFrom(picked.join("\n"), first) : picked.join("\n"),
    first_line: first,
    lines: picked.length,
    version,
  };
}

// numberFrom prefixes each line with its number, so a model reading a region can
// name the line it means without counting.
function numberFrom(text, start) {
  return text
    .split("\n")
    .map((l, i) => `${start + i}\t${l}`)
    .join("\n");
}
