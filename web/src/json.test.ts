import { describe, expect, it } from 'vitest';
import { asJson, jsonTokens } from './json';

describe('asJson', () => {
  it('indents an object, or a string holding one', () => {
    expect(asJson({ a: 1 })).toBe('{\n  "a": 1\n}');
    expect(asJson('[1,2]')).toBe('[\n  1,\n  2\n]');
  });

  it('leaves text, scalars and broken JSON alone', () => {
    expect(asJson('hello')).toBeNull();
    expect(asJson(7)).toBeNull();
    expect(asJson('{"cut": "off')).toBeNull();
  });
});

describe('jsonTokens', () => {
  it('tells keys from string values, and gives the text back', () => {
    const text = asJson({ query: 'a "b": c', n: -1.5e3, ok: true, none: null, list: ['x'] })!;
    const tokens = jsonTokens(text);
    expect(tokens.map((t) => t.text).join('')).toBe(text);
    expect(tokens.filter((t) => t.kind === 'key').map((t) => t.text)).toEqual(['"query"', '"n"', '"ok"', '"none"', '"list"']);
    expect(tokens.filter((t) => t.kind === 'string').map((t) => t.text)).toEqual(['"a \\"b\\": c"', '"x"']);
    expect(tokens.find((t) => t.kind === 'number')?.text).toBe('-1500');
    expect(tokens.filter((t) => t.kind === 'literal').map((t) => t.text)).toEqual(['true', 'null']);
  });
});
