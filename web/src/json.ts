/**
 * JSON shown with its parts told apart: a key, a string, a number, a literal
 * (true, false, null) and punctuation each get a class, so a reader finds the
 * keys at a glance.
 */

export type JsonToken = { kind: 'key' | 'string' | 'number' | 'literal' | 'punct' | 'space'; text: string };

/**
 * The value as indented JSON when it is JSON: an object or an array, or a
 * string that parses as one. Anything else is null, and shows as plain text.
 */
export function asJson(v: unknown): string | null {
  let value = v;
  if (typeof v === 'string') {
    const t = v.trim();
    if (!(t.startsWith('{') || t.startsWith('['))) return null;
    try {
      value = JSON.parse(t);
    } catch {
      return null;
    }
  }
  if (value === null || typeof value !== 'object') return null;
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return null;
  }
}

const TOKEN = /("(?:\\.|[^"\\])*")(\s*:)?|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false|null)\b|([{}[\],:])/g;

/** Indented JSON split into tokens; joined, they give back the text. */
export function jsonTokens(text: string): JsonToken[] {
  const out: JsonToken[] = [];
  let at = 0;
  for (const m of text.matchAll(TOKEN)) {
    if (m.index > at) out.push({ kind: 'space', text: text.slice(at, m.index) });
    if (m[1] !== undefined) {
      out.push({ kind: m[2] ? 'key' : 'string', text: m[1] });
      if (m[2]) out.push({ kind: 'punct', text: m[2] });
    } else if (m[3] !== undefined) out.push({ kind: 'number', text: m[3] });
    else if (m[4] !== undefined) out.push({ kind: 'literal', text: m[4] });
    else out.push({ kind: 'punct', text: m[0] });
    at = m.index + m[0].length;
  }
  if (at < text.length) out.push({ kind: 'space', text: text.slice(at) });
  return out;
}
