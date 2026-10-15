import { describe, expect, it } from 'vitest';
import schema from './schema.d.ts?raw';
import { unused } from './unused';

// The API coverage check (adr/web-ui.md §8): every operation in openapi.yaml is
// used by the UI or listed in unused.ts with the reason it is not, so an
// endpoint added to the API cannot be left out of the UI unnoticed.

/** The UI's own sources, as text: no tests, no generated types. */
const sources = Object.entries(
  import.meta.glob<string>(['../**/*.ts', '../**/*.tsx', '!../**/*.test.ts', '!../**/*.test.tsx', '!../**/*.d.ts'], {
    query: '?raw',
    import: 'default',
    eager: true,
  }),
);

/** Every operation the API declares, as "METHOD /path" → operationId, read from the generated types. */
function declared(): Map<string, string> {
  const paths = schema.slice(schema.indexOf('export interface paths {'), schema.indexOf('export type webhooks'));
  const ops = new Map<string, string>();
  let path = '';
  for (const line of paths.split('\n')) {
    const p = /^ {4}"(\/[^"]*)": \{$/.exec(line);
    if (p) path = p[1];
    const m = /^ {8}(get|put|post|delete|patch): operations\["([^"]+)"\];$/.exec(line);
    if (m && path) ops.set(`${m[1].toUpperCase()} ${path}`, m[2]);
  }
  return ops;
}

/** What the UI calls: the typed client's api.METHOD('/path'), and the raw /api/v1/... URLs it fetches or links to. */
function called(): Set<string> {
  const out = new Set<string>();
  for (const [, text] of sources) {
    for (const m of text.matchAll(/api\.(GET|PUT|POST|DELETE|PATCH)\(\s*'([^']+)'/g)) out.add(`${m[1]} ${m[2]}`);
    for (const m of text.matchAll(/["'`]\/api\/v1(\/[a-z/{}_-]*)/g)) out.add(`GET ${m[1].replace(/\/$/, '')}`);
  }
  return out;
}

describe('API coverage', () => {
  const ops = declared();
  const uses = called();

  it('reads the declared operations and the sources', () => {
    expect(ops.size).toBeGreaterThan(40);
    expect(sources.length).toBeGreaterThan(10);
  });

  it('uses every operation, or says why not', () => {
    const missing = [...ops].filter(([key, id]) => !uses.has(key) && !(id in unused)).map(([key, id]) => `${id} (${key})`);
    expect(missing, 'operations neither used nor listed in unused.ts').toEqual([]);
  });

  it('lists only operations that exist and are really unused', () => {
    const ids = new Map([...ops].map(([key, id]) => [id, key]));
    const stale = Object.keys(unused).filter((id) => !ids.has(id) || uses.has(ids.get(id)!));
    expect(stale, 'entries in unused.ts to remove').toEqual([]);
  });
});
