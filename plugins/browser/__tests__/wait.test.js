import { describe, it, expect, beforeAll, mock } from 'bun:test';

const cfg = { timeout: 30000, allowUrls: [], blockUrls: [], allowPrivate: false };

const page = {
  waitForSelector: async (_sel, _opts) => {},
  waitForFunction: async (_fn, _arg, _opts) => {},
  waitForURL: async (_pattern, _opts) => {},
  url: () => 'https://example.com',
  title: async () => 'Done',
};

mock.module('../config.js', () => ({ config: cfg }));
mock.module('../browser.js', () => ({ ensureBrowser: async () => page }));

let wait;
beforeAll(async () => {
  ({ wait } = await import('../tools/wait.js'));
});

describe('wait', () => {
  it('throws when no condition is provided', async () => {
    await expect(wait({})).rejects.toThrow('at least one of');
  });

  it('waits for a selector', async () => {
    let captured;
    const orig = page.waitForSelector;
    page.waitForSelector = async (sel, opts) => { captured = { sel, opts }; };
    try {
      await wait({ selector: '.ready' });
      expect(captured.sel).toBe('.ready');
      expect(captured.opts.state).toBe('visible');
    } finally {
      page.waitForSelector = orig;
    }
  });

  it('waits for text content', async () => {
    let captured;
    const orig = page.waitForFunction;
    page.waitForFunction = async (_fn, arg, _opts) => { captured = arg; };
    try {
      await wait({ text: 'Order confirmed' });
      expect(captured).toBe('Order confirmed');
    } finally {
      page.waitForFunction = orig;
    }
  });

  it('waits for a URL pattern', async () => {
    let captured;
    const orig = page.waitForURL;
    page.waitForURL = async (pattern, _opts) => { captured = pattern; };
    try {
      await wait({ url_pattern: '**/success**' });
      expect(captured).toBe('**/success**');
    } finally {
      page.waitForURL = orig;
    }
  });

  it('returns url and title on success', async () => {
    const result = JSON.parse(await wait({ selector: '.ok' }));
    expect(result).toMatchObject({ url: 'https://example.com', title: 'Done' });
  });

  it('wraps selector TimeoutError with friendly message', async () => {
    const orig = page.waitForSelector;
    page.waitForSelector = async () => {
      const err = new Error('Timeout');
      err.name = 'TimeoutError';
      throw err;
    };
    try {
      await expect(wait({ selector: '.missing' }))
        .rejects.toThrow('timed out after 30000ms waiting for selector ".missing"');
    } finally {
      page.waitForSelector = orig;
    }
  });

  it('wraps text TimeoutError with friendly message', async () => {
    const orig = page.waitForFunction;
    page.waitForFunction = async () => {
      const err = new Error('Timeout');
      err.name = 'TimeoutError';
      throw err;
    };
    try {
      await expect(wait({ text: 'Order confirmed' }))
        .rejects.toThrow('timed out after 30000ms waiting for text "Order confirmed"');
    } finally {
      page.waitForFunction = orig;
    }
  });

  it('wraps url_pattern TimeoutError with friendly message', async () => {
    const orig = page.waitForURL;
    page.waitForURL = async () => {
      const err = new Error('Timeout');
      err.name = 'TimeoutError';
      throw err;
    };
    try {
      await expect(wait({ url_pattern: '**/done**' }))
        .rejects.toThrow('timed out after 30000ms waiting for URL pattern "**/done**"');
    } finally {
      page.waitForURL = orig;
    }
  });

  it('wraps generic wait errors', async () => {
    const orig = page.waitForSelector;
    page.waitForSelector = async () => { throw new Error('detached'); };
    try {
      await expect(wait({ selector: '.x' })).rejects.toThrow('wait failed');
    } finally {
      page.waitForSelector = orig;
    }
  });
});
