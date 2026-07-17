import { describe, it, expect, beforeAll, mock } from 'bun:test';

const page = {
  evaluate: async (fn) => (typeof fn === 'function' ? fn() : null),
};

mock.module('../config.js', () => ({
  config: { timeout: 30000, allowUrls: [], blockUrls: [], allowPrivate: false },
}));
mock.module('../browser.js', () => ({ ensureBrowser: async () => page }));

let evaluate;
beforeAll(async () => {
  ({ evaluate } = await import('../tools/eval.js'));
});

describe('evaluate (browser_eval)', () => {
  it('throws when script is missing', async () => {
    await expect(evaluate({})).rejects.toThrow('script is required');
  });

  it('returns JSON-encoded result', async () => {
    const orig = page.evaluate;
    page.evaluate = async (_fn) => 42;
    try {
      const result = await evaluate({ script: 'return 42' });
      expect(JSON.parse(result)).toBe(42);
    } finally {
      page.evaluate = orig;
    }
  });

  it('serialises objects', async () => {
    const orig = page.evaluate;
    page.evaluate = async (_fn) => ({ x: 1, y: 2 });
    try {
      const result = await evaluate({ script: 'return {x:1,y:2}' });
      expect(JSON.parse(result)).toEqual({ x: 1, y: 2 });
    } finally {
      page.evaluate = orig;
    }
  });

  it('returns "null" for undefined result', async () => {
    const orig = page.evaluate;
    page.evaluate = async (_fn) => undefined;
    try {
      const result = await evaluate({ script: 'return undefined' });
      expect(result).toBe('null');
    } finally {
      page.evaluate = orig;
    }
  });

  it('wraps page.evaluate errors', async () => {
    const orig = page.evaluate;
    page.evaluate = async () => { throw new Error('ReferenceError: x is not defined'); };
    try {
      await expect(evaluate({ script: 'return x' })).rejects.toThrow('eval failed');
    } finally {
      page.evaluate = orig;
    }
  });
});
