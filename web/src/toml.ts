import type { JsonToken } from './json';

/**
 * TOML split into the same parts as JSON (json.ts), so a configuration shows
 * its keys the way a tool's JSON does: a table header and a key are keys;
 * strings, numbers and booleans are values.
 */
export function tomlTokens(text: string): JsonToken[] {
  const out: JsonToken[] = [];
  for (const [n, line] of text.split('\n').entries()) {
    if (n > 0) out.push({ kind: 'space', text: '\n' });
    const header = /^(\s*)(\[\[?[^\]]+\]\]?)(.*)$/.exec(line);
    if (header) {
      if (header[1]) out.push({ kind: 'space', text: header[1] });
      out.push({ kind: 'key', text: header[2] });
      if (header[3]) out.push({ kind: 'space', text: header[3] });
      continue;
    }
    const pair = /^(\s*)([^=\s][^=]*?)(\s*=\s*)(.*)$/.exec(line);
    if (!pair) {
      if (line) out.push({ kind: 'space', text: line });
      continue;
    }
    if (pair[1]) out.push({ kind: 'space', text: pair[1] });
    out.push({ kind: 'key', text: pair[2] });
    out.push({ kind: 'punct', text: pair[3] });
    out.push(...valueTokens(pair[4]));
  }
  return out;
}

const VALUE = /("(?:\\.|[^"\\])*")|(-?\d[\d_]*(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false)\b|([[\]{},])/g;

function valueTokens(text: string): JsonToken[] {
  const out: JsonToken[] = [];
  let at = 0;
  for (const m of text.matchAll(VALUE)) {
    if (m.index > at) out.push({ kind: 'space', text: text.slice(at, m.index) });
    if (m[1] !== undefined) out.push({ kind: 'string', text: m[1] });
    else if (m[2] !== undefined) out.push({ kind: 'number', text: m[2] });
    else if (m[3] !== undefined) out.push({ kind: 'literal', text: m[3] });
    else out.push({ kind: 'punct', text: m[0] });
    at = m.index + m[0].length;
  }
  if (at < text.length) out.push({ kind: 'space', text: text.slice(at) });
  return out;
}
