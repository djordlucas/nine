import { describe, expect, it } from 'vitest';
import { argsTemplate, groupTools, parseArgs, type Tool } from './tools';

const tool = (name: string, kind: string, over: Partial<Tool> = {}): Tool => ({ name, kind, description: '', ...over });

describe('groupTools', () => {
  const tools = [tool('read_file', 'core'), tool('shell', 'plugin', { plugin: 'shell' }), tool('digest', 'generated'), tool('b', 'core')];

  it('groups by kind, written-by-Nine first, names sorted', () => {
    expect(groupTools(tools).map(([k, ts]) => [k, ts.map((t) => t.name)])).toEqual([
      ['generated', ['digest']],
      ['plugin', ['shell']],
      ['core', ['b', 'read_file']],
    ]);
  });

  it('filters on name, description and plugin', () => {
    expect(groupTools(tools, 'SHELL').map(([k]) => k)).toEqual(['plugin']);
    expect(groupTools(tools, 'nothing')).toEqual([]);
  });
});

describe('parseArgs', () => {
  it('takes an object, and empty as none', () => {
    expect(parseArgs('{"a":1}')).toEqual({ args: { a: 1 } });
    expect(parseArgs('  ')).toEqual({ args: {} });
  });

  it('says why anything else is refused', () => {
    expect('error' in parseArgs('{a')).toBe(true);
    expect(parseArgs('[1]')).toEqual({ error: 'The arguments must be a JSON object.' });
  });
});

describe('argsTemplate', () => {
  it('fills the required properties with empty values of their type', () => {
    const schema = {
      type: 'object',
      required: ['path', 'n', 'all'],
      properties: { path: { type: 'string' }, n: { type: 'integer' }, all: { type: 'boolean' }, extra: { type: 'string' } },
    };
    expect(JSON.parse(argsTemplate(schema))).toEqual({ path: '', n: 0, all: false });
    expect(argsTemplate(undefined)).toBe('{}');
  });
});
