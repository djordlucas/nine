import { describe, it, expect, beforeAll, mock } from 'bun:test';

let resetCalled = false;

mock.module('../config.js', () => ({
  config: { timeout: 30000, allowUrls: [], blockUrls: [], allowPrivate: false },
}));
mock.module('../browser.js', () => ({
  resetBrowser: async () => { resetCalled = true; },
  ensureBrowser: async () => ({}),
  getBrowser: () => null,
  getPage: () => null,
}));

let reset;
beforeAll(async () => {
  ({ reset } = await import('../tools/reset.js'));
});

describe('reset', () => {
  it('returns ok', async () => {
    const result = await reset({});
    expect(result).toBe('ok');
  });

  it('calls resetBrowser', async () => {
    resetCalled = false;
    await reset({});
    expect(resetCalled).toBe(true);
  });
});
