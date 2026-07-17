import { describe, it, expect, beforeAll, mock } from 'bun:test';

const MOCK_ELEMENTS = [
  { innerText: 'Alpha', getAttribute: (a) => `${a}-1` },
  { innerText: 'Beta',  getAttribute: (a) => `${a}-2` },
];

const page = {
  evaluate: async (_fn) => 'full body text',
  $$eval: async (_selector, fn, ...args) => fn(MOCK_ELEMENTS, ...args),
};

mock.module('../config.js', () => ({ config: { timeout: 30000, allowUrls: [], blockUrls: [], allowPrivate: false } }));
mock.module('../browser.js', () => ({ ensureBrowser: async () => page }));

let extract;
beforeAll(async () => {
  ({ extract } = await import('../tools/extract.js'));
});

describe('extract', () => {
  it('returns full body text when no selector given', async () => {
    const result = await extract({});
    expect(result).toBe('full body text');
  });

  it('returns newline-joined text for matched elements', async () => {
    const result = await extract({ selector: 'p' });
    expect(result).toBe('Alpha\nBeta');
  });

  it('returns JSON array with as_json=true', async () => {
    const result = await extract({ selector: 'p', as_json: true });
    expect(JSON.parse(result)).toEqual(['Alpha', 'Beta']);
  });

  it('returns attribute values when attribute is specified', async () => {
    const result = await extract({ selector: 'a', attribute: 'href' });
    expect(result).toBe('href-1\nhref-2');
  });

  it('returns attribute values as JSON with as_json=true', async () => {
    const result = await extract({ selector: 'a', attribute: 'href', as_json: true });
    expect(JSON.parse(result)).toEqual(['href-1', 'href-2']);
  });

  it('truncates output at max_chars', async () => {
    const result = await extract({ max_chars: 4 });
    expect(result).toContain('[truncated at 4 chars]');
    expect(result.startsWith('full')).toBe(true);
  });

  it('does not truncate when output fits within max_chars', async () => {
    const result = await extract({ max_chars: 10000 });
    expect(result).toBe('full body text');
  });
});
