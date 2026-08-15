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

let checkUrl, isBlockedUrl;
beforeAll(async () => {
  ({ checkUrl, isBlockedUrl } = await import('../security.js'));
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

describe('checkUrl — link-local and metadata endpoints', () => {
  // 169.254.169.254 serves instance credentials on AWS, GCP and Azure to
  // anything that asks. An agent told to "check this URL" is anything.
  it('blocks the cloud metadata address', () => {
    expect(() => checkUrl('http://169.254.169.254/latest/meta-data/')).toThrow(/private host/);
  });

  it('blocks the rest of link-local, not just the metadata address', () => {
    expect(() => checkUrl('http://169.254.1.1/')).toThrow(/private host/);
  });

  it('blocks the metadata hostnames that alias it', () => {
    expect(() => checkUrl('http://metadata.google.internal/computeMetadata/v1/')).toThrow(/private host/);
    expect(() => checkUrl('http://metadata/')).toThrow(/private host/);
  });

  it('blocks 0.0.0.0, which routes to the local host', () => {
    expect(() => checkUrl('http://0.0.0.0:8080/')).toThrow(/private host/);
  });

  it('blocks unique-local IPv6 across the whole fc00::/7', () => {
    expect(() => checkUrl('http://[fd00::1]/')).toThrow(/private host/);
    expect(() => checkUrl('http://[fc00::1]/')).toThrow(/private host/);
  });

  it('blocks link-local IPv6 beyond the fe80: prefix', () => {
    expect(() => checkUrl('http://[fe81::1]/')).toThrow(/private host/);
  });

  it('still allows a public address that merely starts with similar digits', () => {
    expect(() => checkUrl('https://169.255.1.1/')).not.toThrow();
    expect(() => checkUrl('https://16.254.1.1/')).not.toThrow();
  });

  it('honours allowPrivate for deliberate local browsing', () => {
    cfg.allowPrivate = true;
    expect(() => checkUrl('http://169.254.169.254/')).not.toThrow();
    cfg.allowPrivate = false;
  });
});

describe('checkUrl — IPv4-mapped IPv6', () => {
  // The browser normalizes [::ffff:169.254.169.254] to [::ffff:a9fe:a9fe],
  // which matches no IPv4 rule — so the mapped form bypassed every one of them
  // and still reached the metadata endpoint.
  it('blocks the metadata address written as a mapped IPv6 literal', () => {
    expect(() => checkUrl('http://[::ffff:169.254.169.254]/')).toThrow(/private host/);
  });

  it('blocks mapped loopback', () => {
    expect(() => checkUrl('http://[::ffff:127.0.0.1]/')).toThrow(/private host/);
  });

  it('blocks mapped RFC1918', () => {
    expect(() => checkUrl('http://[::ffff:192.168.1.1]/')).toThrow(/private host/);
    expect(() => checkUrl('http://[::ffff:10.0.0.1]/')).toThrow(/private host/);
  });

  it('still allows a mapped public address', () => {
    expect(() => checkUrl('http://[::ffff:93.184.216.34]/')).not.toThrow();
  });
});

describe('checkUrl — full fe80::/10', () => {
  it('blocks across the whole range, not just fe8x', () => {
    for (const h of ['fe80::1', 'fe8f::1', 'fe90::1', 'feaf::1', 'febf::1']) {
      expect(() => checkUrl(`http://[${h}]/`)).toThrow(/private host/);
    }
  });

  it('does not over-block fec0:: and above', () => {
    expect(() => checkUrl('http://[fec0::1]/')).not.toThrow();
  });
});

describe('isBlockedUrl — predicate form for the navigation guard', () => {
  it('mirrors checkUrl without throwing', () => {
    expect(isBlockedUrl('https://example.com')).toBe(false);
    expect(isBlockedUrl('http://169.254.169.254/')).toBe(true);
  });

  it('treats an unreadable URL as blocked', () => {
    expect(isBlockedUrl('not a url')).toBe(true);
  });
});
