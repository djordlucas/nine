import { describe, it, expect, beforeAll, mock } from 'bun:test';

const cfg = { timeout: 30000, allowUrls: [], blockUrls: [], allowPrivate: false };

const page = {
  fill: async (_sel, _val, _opts) => {},
  type: async (_sel, _val, _opts) => {},
};

mock.module('../config.js', () => ({ config: cfg }));
mock.module('../browser.js', () => ({ ensureBrowser: async () => page }));

let fill;
beforeAll(async () => {
  ({ fill } = await import('../tools/fill.js'));
});

describe('fill', () => {
  it('throws when selector is missing', async () => {
    await expect(fill({ value: 'hello' })).rejects.toThrow('selector is required');
  });

  it('throws when value is missing', async () => {
    await expect(fill({ selector: '#input' })).rejects.toThrow('value is required');
  });

  it('uses page.fill (clears field) by default', async () => {
    let method;
    const origFill = page.fill;
    const origType = page.type;
    page.fill = async () => { method = 'fill'; };
    page.type = async () => { method = 'type'; };
    try {
      await fill({ selector: '#input', value: 'hello' });
      expect(method).toBe('fill');
    } finally {
      page.fill = origFill;
      page.type = origType;
    }
  });

  it('uses page.type when clear_first is false', async () => {
    let method;
    const origFill = page.fill;
    const origType = page.type;
    page.fill = async () => { method = 'fill'; };
    page.type = async () => { method = 'type'; };
    try {
      await fill({ selector: '#input', value: 'hello', clear_first: false });
      expect(method).toBe('type');
    } finally {
      page.fill = origFill;
      page.type = origType;
    }
  });

  it('returns ok on success', async () => {
    const result = await fill({ selector: '#input', value: 'hello' });
    expect(result).toBe('ok');
  });

  it('wraps TimeoutError with a friendly message', async () => {
    const orig = page.fill;
    page.fill = async () => {
      const err = new Error('Timeout');
      err.name = 'TimeoutError';
      throw err;
    };
    try {
      await expect(fill({ selector: '#missing', value: 'x' }))
        .rejects.toThrow('element not found within');
    } finally {
      page.fill = orig;
    }
  });

  it('wraps generic fill errors', async () => {
    const orig = page.fill;
    page.fill = async () => { throw new Error('not an input'); };
    try {
      await expect(fill({ selector: 'div', value: 'x' }))
        .rejects.toThrow('fill failed');
    } finally {
      page.fill = orig;
    }
  });
});
