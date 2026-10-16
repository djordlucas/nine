import { describe, expect, it } from 'vitest';
import { asText, cell, crumbs, dirHref, fieldValue, fileHref, join, parent, version } from './data';

describe('workspace paths', () => {
  it('opens a directory or a file by query', () => {
    expect(dirHref('')).toBe('/data/files');
    expect(dirHref('notes/2026')).toBe('/data/files?dir=notes%2F2026');
    expect(fileHref('notes/a b.md')).toBe('/data/files?file=notes%2Fa%20b.md');
  });

  it('finds the parent, joins a name, and lists the crumbs', () => {
    expect(parent('notes/today.md')).toBe('notes');
    expect(parent('today.md')).toBe('');
    expect(join('', '/x.md')).toBe('x.md');
    expect(join('notes', 'sub/x.md')).toBe('notes/sub/x.md');
    expect(crumbs('notes/2026').map((c) => c.path)).toEqual(['', 'notes', 'notes/2026']);
  });

  it('reads an ETag back as a version', () => {
    expect(version('"abc-12"')).toBe('abc-12');
    expect(version('W/"abc"')).toBe('abc');
    expect(version(null)).toBeUndefined();
  });
});

describe('asText', () => {
  it('takes UTF-8 text and refuses binary', () => {
    expect(asText(new TextEncoder().encode('héllo').buffer as ArrayBuffer)).toBe('héllo');
    expect(asText(new Uint8Array([0xff, 0xfe, 0x00]).buffer)).toBeNull();
    expect(asText(new Uint8Array([104, 0, 105]).buffer)).toBeNull();
  });
});

describe('database values', () => {
  it('shows NULL, JSON and text', () => {
    expect(cell(null)).toBe('NULL');
    expect(cell({ a: 1 })).toBe('{"a":1}');
    expect(cell(7)).toBe('7');
  });

  it('types a field by its column', () => {
    expect(fieldValue('7', false, 'INTEGER')).toBe(7);
    expect(fieldValue('1.5', false, 'real')).toBe(1.5);
    expect(fieldValue('7', false, 'TEXT')).toBe('7');
    expect(fieldValue('abc', false, 'INTEGER')).toBe('abc');
    expect(fieldValue('', false, 'INTEGER')).toBe('');
    expect(fieldValue('x', true, 'TEXT')).toBeNull();
  });
});
