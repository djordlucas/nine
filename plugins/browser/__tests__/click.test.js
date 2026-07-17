import { describe, it, expect, beforeAll, mock } from 'bun:test';

const cfg = { timeout: 30000, allowUrls: [], blockUrls: [], allowPrivate: false };

const page = {
  click: async (_selector, _opts) => {},
  waitForNavigation: async (_opts) => {},
  url: () => 'https://example.com/after',
  title: async () => 'After Click',
};

mock.module('../config.js', () => ({ config: cfg }));
mock.module('../browser.js', () => ({ ensureBrowser: async () => page }));

let click;
beforeAll(async () => {
  ({ click } = await import('../tools/click.js'));
});

describe('click', () => {
  it('throws when selector is missing', async () => {
    await expect(click({})).rejects.toThrow('selector is required');
  });

  it('returns url and title after click', async () => {
    const result = JSON.parse(await click({ selector: 'button' }));
    expect(result.url).toBe('https://example.com/after');
    expect(result.title).toBe('After Click');
  });

  it('wraps TimeoutError with a friendly message', async () => {
    const orig = page.click;
    page.click = async () => {
      const err = new Error('Timeout');
      err.name = 'TimeoutError';
      throw err;
    };
    try {
      await expect(click({ selector: '.missing' })).rejects.toThrow('element not found within');
    } finally {
      page.click = orig;
    }
  });

  it('wraps generic click errors', async () => {
    const orig = page.click;
    page.click = async () => { throw new Error('intercept error'); };
    try {
      await expect(click({ selector: 'button' })).rejects.toThrow('click failed');
    } finally {
      page.click = orig;
    }
  });

  it('waits for navigation when wait_for_navigation is true', async () => {
    let navigated = false;
    const origNav = page.waitForNavigation;
    page.waitForNavigation = async () => { navigated = true; };
    try {
      await click({ selector: 'a', wait_for_navigation: true });
      expect(navigated).toBe(true);
    } finally {
      page.waitForNavigation = origNav;
    }
  });

  it('does not wait for navigation by default', async () => {
    let navigated = false;
    const origNav = page.waitForNavigation;
    page.waitForNavigation = async () => { navigated = true; };
    try {
      await click({ selector: 'button' });
      expect(navigated).toBe(false);
    } finally {
      page.waitForNavigation = origNav;
    }
  });
});
