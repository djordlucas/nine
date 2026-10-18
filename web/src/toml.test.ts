import { describe, expect, it } from 'vitest';
import { tomlTokens } from './toml';

describe('tomlTokens', () => {
  it('marks headers and keys, types values, and gives the text back', () => {
    const text = 'schema_version = 2\n\n[llm]\nmodel = "qwen3.5:4b"\nrequire_approval = ["shell", "write_file"]\nenabled = true';
    const tokens = tomlTokens(text);
    expect(tokens.map((t) => t.text).join('')).toBe(text);
    expect(tokens.filter((t) => t.kind === 'key').map((t) => t.text)).toEqual([
      'schema_version',
      '[llm]',
      'model',
      'require_approval',
      'enabled',
    ]);
    expect(tokens.filter((t) => t.kind === 'string').map((t) => t.text)).toEqual(['"qwen3.5:4b"', '"shell"', '"write_file"']);
    expect(tokens.find((t) => t.kind === 'number')?.text).toBe('2');
    expect(tokens.find((t) => t.kind === 'literal')?.text).toBe('true');
  });
});
