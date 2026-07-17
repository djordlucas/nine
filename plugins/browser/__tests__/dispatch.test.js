import { describe, it, expect } from 'bun:test';

// Minimal self-contained RPC dispatcher extracted from index.js logic.
// Tests routing, error handling, and describe output without touching Playwright.

const TOOL_DEFS = [
  { name: 'browser_navigate', description: 'Navigate', input_schema: { type: 'object', required: ['url'], properties: { url: { type: 'string' } } } },
  { name: 'browser_status',   description: 'Status',   input_schema: { type: 'object', properties: {} } },
];

function rpcError(code, message) {
  const err = new Error(message);
  err.__rpc = true;
  err.code = code;
  return err;
}

const HANDLERS = {
  browser_navigate: async (args) => {
    if (!args.url) throw new Error('url is required');
    return JSON.stringify({ url: args.url, title: 'Test', status: 200 });
  },
  browser_status: async (_args) => JSON.stringify({ open: false }),
};

async function dispatch(req) {
  const { id, method, params } = req;
  try {
    let result;
    if (method === 'plugin.describe') {
      result = { tools: TOOL_DEFS };
    } else if (method === 'plugin.call') {
      const handler = HANDLERS[params.tool];
      if (!handler) throw rpcError(-32601, 'unknown tool: ' + params.tool);
      result = { output: await handler(params.args ?? {}) };
    } else {
      throw rpcError(-32601, 'method not found: ' + method);
    }
    return { jsonrpc: '2.0', id, result };
  } catch (err) {
    const isRpc = err && err.__rpc;
    return {
      jsonrpc: '2.0', id,
      error: { code: isRpc ? err.code : -32603, message: String(err.message || err) },
    };
  }
}

describe('RPC dispatch', () => {
  describe('plugin.describe', () => {
    it('returns the tools array', async () => {
      const resp = await dispatch({ id: 1, method: 'plugin.describe', params: {} });
      expect(resp.result.tools).toBeArray();
      expect(resp.result.tools.length).toBeGreaterThan(0);
      expect(resp.result.tools[0]).toHaveProperty('name');
      expect(resp.result.tools[0]).toHaveProperty('input_schema');
    });

    it('echoes the request id', async () => {
      const resp = await dispatch({ id: 99, method: 'plugin.describe', params: {} });
      expect(resp.id).toBe(99);
    });
  });

  describe('plugin.call — success', () => {
    it('routes to the correct handler', async () => {
      const resp = await dispatch({ id: 2, method: 'plugin.call', params: { tool: 'browser_navigate', args: { url: 'https://example.com' } } });
      expect(resp.result.output).toBeDefined();
      const out = JSON.parse(resp.result.output);
      expect(out.url).toBe('https://example.com');
    });

    it('passes args to the handler', async () => {
      const resp = await dispatch({ id: 3, method: 'plugin.call', params: { tool: 'browser_status', args: {} } });
      expect(JSON.parse(resp.result.output).open).toBe(false);
    });

    it('handles missing args gracefully (defaults to {})', async () => {
      const resp = await dispatch({ id: 4, method: 'plugin.call', params: { tool: 'browser_status' } });
      expect(resp.result).toBeDefined();
    });
  });

  describe('plugin.call — errors', () => {
    it('returns -32601 for an unknown tool', async () => {
      const resp = await dispatch({ id: 5, method: 'plugin.call', params: { tool: 'no_such_tool', args: {} } });
      expect(resp.error.code).toBe(-32601);
      expect(resp.error.message).toContain('unknown tool');
    });

    it('returns -32603 for a handler error', async () => {
      const resp = await dispatch({ id: 6, method: 'plugin.call', params: { tool: 'browser_navigate', args: {} } });
      expect(resp.error.code).toBe(-32603);
      expect(resp.error.message).toContain('url is required');
    });
  });

  describe('unknown method', () => {
    it('returns -32601 for an unrecognised method', async () => {
      const resp = await dispatch({ id: 7, method: 'plugin.unknown', params: {} });
      expect(resp.error.code).toBe(-32601);
      expect(resp.error.message).toContain('method not found');
    });
  });
});
