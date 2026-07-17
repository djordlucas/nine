import { describe, it, expect, beforeAll, mock } from 'bun:test';

const cfg = {
  timeout: 30000,
  viewport: { width: 1280, height: 800 },
  allowUrls: [], blockUrls: [], allowPrivate: false,
};

// Mutable browser state so tests can simulate connected / disconnected.
const browserState = {
  browser: null,
  page: null,
};

mock.module('../config.js', () => ({ config: cfg }));
mock.module('../browser.js', () => ({
  getBrowser: () => browserState.browser,
  getPage:    () => browserState.page,
  ensureBrowser: async () => browserState.page,
}));

let status;
beforeAll(async () => {
  ({ status } = await import('../tools/status.js'));
});

describe('status', () => {
  it('returns {open:false} when no browser is running', async () => {
    browserState.browser = null;
    browserState.page = null;
    const result = JSON.parse(await status({}));
    expect(result).toEqual({ open: false });
  });

  it('returns {open:false} when browser is disconnected', async () => {
    browserState.browser = { isConnected: () => false };
    browserState.page = { url: () => 'https://x.com', title: async () => 'X' };
    const result = JSON.parse(await status({}));
    expect(result).toEqual({ open: false });
  });

  it('returns url, title, and viewport when browser is open', async () => {
    browserState.browser = { isConnected: () => true };
    browserState.page = {
      url: () => 'https://example.com',
      title: async () => 'Example',
    };
    const result = JSON.parse(await status({}));
    expect(result.open).toBe(true);
    expect(result.url).toBe('https://example.com');
    expect(result.title).toBe('Example');
    expect(result.viewport).toEqual({ width: 1280, height: 800 });
  });
});
