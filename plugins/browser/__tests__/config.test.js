import { describe, it, expect, beforeAll } from 'bun:test';

// Import config with no BROWSER_* env vars set so we get true defaults.
let config;
beforeAll(async () => {
  ({ config } = await import('../config.js'));
});

describe('config defaults', () => {
  it('headless defaults to true', () => {
    expect(config.headless).toBe(true);
  });

  it('timeout defaults to 30000', () => {
    expect(config.timeout).toBe(30000);
  });

  it('viewport defaults to 1280x800', () => {
    expect(config.viewport).toEqual({ width: 1280, height: 800 });
  });

  it('allowUrls defaults to empty array', () => {
    expect(config.allowUrls).toEqual([]);
  });

  it('blockUrls defaults to empty array', () => {
    expect(config.blockUrls).toEqual([]);
  });

  it('allowPrivate defaults to false', () => {
    expect(config.allowPrivate).toBe(false);
  });

  it('maxScreenshotBytes defaults to 3000000', () => {
    expect(config.maxScreenshotBytes).toBe(3_000_000);
  });
});
