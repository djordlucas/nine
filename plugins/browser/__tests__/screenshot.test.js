import { describe, it, expect, beforeAll, mock } from 'bun:test';

const cfg = { timeout: 30000, maxScreenshotBytes: 100, allowUrls: [], blockUrls: [], allowPrivate: false };

const SMALL_BUF = Buffer.alloc(50, 0);   // 50 bytes — under limit
const LARGE_BUF = Buffer.alloc(200, 0);  // 200 bytes — over 100-byte limit

const page = {
  screenshot: async (_opts) => SMALL_BUF,
  viewportSize: () => ({ width: 1280, height: 800 }),
  locator: (_selector) => ({
    first: () => ({
      screenshot: async (_opts) => SMALL_BUF,
    }),
  }),
};

mock.module('../config.js', () => ({ config: cfg }));
mock.module('../browser.js', () => ({ ensureBrowser: async () => page }));

let screenshot;
beforeAll(async () => {
  ({ screenshot } = await import('../tools/screenshot.js'));
});

describe('screenshot', () => {
  it('returns PNG by default', async () => {
    const result = JSON.parse(await screenshot({}));
    expect(result.type).toBe('png');
    expect(result.data).toBe(SMALL_BUF.toString('base64'));
    expect(result.width).toBe(1280);
    expect(result.height).toBe(800);
  });

  it('returns JPEG when quality is specified', async () => {
    const result = JSON.parse(await screenshot({ quality: 80 }));
    expect(result.type).toBe('jpeg');
  });

  it('falls back to JPEG when PNG exceeds the size limit', async () => {
    // First call returns an oversized PNG; second (JPEG fallback) returns small buf.
    let calls = 0;
    const orig = page.screenshot;
    page.screenshot = async (opts) => {
      calls++;
      return calls === 1 ? LARGE_BUF : SMALL_BUF;
    };
    try {
      const result = JSON.parse(await screenshot({}));
      expect(result.type).toBe('jpeg');
      expect(calls).toBe(2);
    } finally {
      page.screenshot = orig;
    }
  });

  it('throws when JPEG fallback is also too large', async () => {
    const orig = page.screenshot;
    page.screenshot = async () => LARGE_BUF;
    try {
      await expect(screenshot({})).rejects.toThrow('screenshot too large');
    } finally {
      page.screenshot = orig;
    }
  });

  it('uses the element screenshot when selector is provided', async () => {
    let usedSelector;
    const origLocator = page.locator;
    page.locator = (sel) => {
      usedSelector = sel;
      return { first: () => ({ screenshot: async () => SMALL_BUF }) };
    };
    try {
      await screenshot({ selector: '#content' });
      expect(usedSelector).toBe('#content');
    } finally {
      page.locator = origLocator;
    }
  });
});
