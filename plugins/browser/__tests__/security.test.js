import { describe, it, expect, beforeAll, mock } from 'bun:test';

// Mutable config so individual tests can adjust allowUrls/blockUrls/allowPrivate.
const cfg = {
  allowUrls: [],
  blockUrls: [],
  allowPrivate: false,
  timeout: 30000,
  viewport: { width: 1280, height: 800 },
  maxScreenshotBytes: 3_000_000,
  headless: true,
};

mock.module('../config.js', () => ({ config: cfg }));

let checkUrl;
beforeAll(async () => {
  ({ checkUrl } = await import('../security.js'));
});

describe('checkUrl — valid URLs', () => {
  it('passes a plain HTTPS URL', () => {
    expect(() => checkUrl('https://example.com')).not.toThrow();
  });

  it('passes an HTTP URL with path and query', () => {
    expect(() => checkUrl('http://example.com/path?q=1')).not.toThrow();
  });
});

describe('checkUrl — invalid URLs', () => {
  it('throws -32602 for a non-URL string', () => {
    try {
      checkUrl('not a url');
      expect.unreachable('should have thrown');
    } catch (err) {
      expect(err.code).toBe(-32602);
      expect(err.message).toContain('invalid URL');
    }
  });

  it('throws -32602 for an empty string', () => {
    try {
      checkUrl('');
      expect.unreachable('should have thrown');
    } catch (err) {
      expect(err.code).toBe(-32602);
    }
  });
});

describe('checkUrl — private IP blocking', () => {
  it('blocks localhost', () => {
    expect(() => checkUrl('http://localhost/')).toThrow('blocked: private host localhost');
  });

  it('blocks 127.0.0.1', () => {
    expect(() => checkUrl('http://127.0.0.1/')).toThrow('private host');
  });

  it('blocks 10.x.x.x', () => {
    expect(() => checkUrl('http://10.0.0.1/')).toThrow('private host');
  });

  it('blocks 192.168.x.x', () => {
    expect(() => checkUrl('http://192.168.1.1/')).toThrow('private host');
  });

  it('blocks 172.16.x.x', () => {
    expect(() => checkUrl('http://172.16.0.1/')).toThrow('private host');
  });

  it('blocks IPv6 loopback ::1', () => {
    expect(() => checkUrl('http://[::1]/')).toThrow('private host');
  });

  it('allows private IPs when allowPrivate is set', () => {
    cfg.allowPrivate = true;
    try {
      expect(() => checkUrl('http://127.0.0.1/')).not.toThrow();
    } finally {
      cfg.allowPrivate = false;
    }
  });
});

describe('checkUrl — allowUrls list', () => {
  it('passes a URL that matches the allowlist', () => {
    cfg.allowUrls = ['https://allowed.com/**'];
    try {
      expect(() => checkUrl('https://allowed.com/page')).not.toThrow();
    } finally {
      cfg.allowUrls = [];
    }
  });

  it('blocks a URL not in the allowlist', () => {
    cfg.allowUrls = ['https://allowed.com/**'];
    try {
      expect(() => checkUrl('https://other.com/page')).toThrow('not in allowlist');
    } finally {
      cfg.allowUrls = [];
    }
  });
});

describe('checkUrl — blockUrls list', () => {
  it('blocks a URL that matches the blocklist', () => {
    cfg.blockUrls = ['**/admin/**'];
    try {
      expect(() => checkUrl('https://example.com/admin/panel')).toThrow('matches blocklist');
    } finally {
      cfg.blockUrls = [];
    }
  });

  it('passes a URL that does not match the blocklist', () => {
    cfg.blockUrls = ['**/admin/**'];
    try {
      expect(() => checkUrl('https://example.com/public')).not.toThrow();
    } finally {
      cfg.blockUrls = [];
    }
  });
});
