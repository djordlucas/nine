import { describe, it, expect, beforeAll, mock } from 'bun:test';

const cfg = { timeout: 30000, allowUrls: [], blockUrls: [], allowPrivate: false };

// Page state object — tests may replace individual methods to simulate errors.
const page = {
  goto: async (_url, _opts) => ({ status: () => 200 }),
  url: () => 'https://example.com/final',
  title: async () => 'Example Page',
};

mock.module('../config.js', () => ({ config: cfg }));
mock.module('../browser.js', () => ({ ensureBrowser: async () => page }));

let navigate;
beforeAll(async () => {
  ({ navigate } = await import('../tools/navigate.js'));
});

describe('navigate', () => {
  it('returns url, title, and status on success', async () => {
    const raw = await navigate({ url: 'https://example.com' });
    const result = JSON.parse(raw);
    expect(result.url).toBe('https://example.com/final');
    expect(result.title).toBe('Example Page');
    expect(result.status).toBe(200);
  });

  it('throws when url is missing', async () => {
    await expect(navigate({})).rejects.toThrow('url is required');
  });

  it('rejects private IP addresses', async () => {
    await expect(navigate({ url: 'http://127.0.0.1/' })).rejects.toThrow('blocked: private host');
  });

  it('wraps TimeoutError with a friendly message', async () => {
    const orig = page.goto;
    page.goto = async () => {
      const err = new Error('Timeout exceeded');
      err.name = 'TimeoutError';
      throw err;
    };
    try {
      await expect(navigate({ url: 'https://example.com' }))
        .rejects.toThrow('navigation timed out after');
    } finally {
      page.goto = orig;
    }
  });

  it('wraps generic navigation errors', async () => {
    const orig = page.goto;
    page.goto = async () => { throw new Error('net::ERR_CONNECTION_REFUSED'); };
    try {
      await expect(navigate({ url: 'https://example.com' }))
        .rejects.toThrow('navigation failed');
    } finally {
      page.goto = orig;
    }
  });

  it('passes wait_until and timeout to page.goto', async () => {
    let capturedOpts;
    const orig = page.goto;
    page.goto = async (_url, opts) => { capturedOpts = opts; return { status: () => 200 }; };
    try {
      await navigate({ url: 'https://example.com', wait_until: 'networkidle', timeout: 5000 });
      expect(capturedOpts).toMatchObject({ waitUntil: 'networkidle', timeout: 5000 });
    } finally {
      page.goto = orig;
    }
  });

  it('returns null status when response is absent', async () => {
    const orig = page.goto;
    page.goto = async () => null;
    try {
      const result = JSON.parse(await navigate({ url: 'https://example.com' }));
      expect(result.status).toBeNull();
    } finally {
      page.goto = orig;
    }
  });
});
