import http from 'http';
import fs from 'fs';
import { TOOL_DEFS } from './tools/defs.js';
import { navigate } from './tools/navigate.js';
import { screenshot } from './tools/screenshot.js';
import { extract } from './tools/extract.js';
import { click } from './tools/click.js';
import { fill } from './tools/fill.js';
import { evaluate } from './tools/eval.js';
import { wait } from './tools/wait.js';
import { status } from './tools/status.js';
import { reset } from './tools/reset.js';
import { closeBrowser } from './browser.js';

// Native plugin wire-contract version, mirroring plugin.ProtocolVersion in
// internal/plugin/contract.go. Go plugins get this stamped by plugin.Serve; this
// one is Node, so it declares the version itself. The daemon rejects a plugin
// that reports nothing (see checkProtocolVersion), so bump this in step.
const PROTOCOL_VERSION = 1;

const HANDLERS = {
  browser_navigate:   (args) => navigate(args),
  browser_screenshot: (args) => screenshot(args),
  browser_extract:    (args) => extract(args),
  browser_click:      (args) => click(args),
  browser_fill:       (args) => fill(args),
  browser_eval:       (args) => evaluate(args),
  browser_wait:       (args) => wait(args),
  browser_status:     (args) => status(args),
  browser_reset:      (args) => reset(args),
};

function rpcError(code, message) {
  const err = new Error(message);
  err.__rpc = true;
  err.code = code;
  return err;
}

const SOCKET = process.env.NINE_PLUGIN_SOCKET;
if (!SOCKET) {
  console.error('browser: NINE_PLUGIN_SOCKET not set');
  process.exit(1);
}
try { fs.unlinkSync(SOCKET); } catch { /* no stale socket */ }

function reply(res, obj) {
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(obj));
}

// The browser holds a single shared page, so calls must stay serial: it
// advertises max_concurrent = 1, and the daemon bounds connections to match.
const server = http.createServer((req, res) => {
  if (req.method !== 'POST' || req.url !== '/rpc') {
    res.writeHead(404);
    res.end();
    return;
  }
  let body = '';
  req.on('data', (chunk) => { body += chunk; });
  req.on('end', async () => {
    let request;
    try { request = JSON.parse(body); } catch { return reply(res, { error: { code: -32700, message: 'parse error' } }); }

    const { method, params } = request;
    try {
      let result;
      if (method === 'plugin.describe') {
        result = { protocol_version: PROTOCOL_VERSION, tools: TOOL_DEFS, max_concurrent: 1 };
      } else if (method === 'plugin.call') {
        const handler = HANDLERS[params.tool];
        if (!handler) throw rpcError(-32601, 'unknown tool: ' + params.tool);
        result = { output: await handler(params.args ?? {}) };
      } else {
        throw rpcError(-32601, 'method not found: ' + method);
      }
      reply(res, { result });
    } catch (err) {
      const isRpc = err && err.__rpc;
      reply(res, { error: { code: isRpc ? err.code : -32603, message: String(err.message || err) } });
    }
  });
});

server.listen(SOCKET);

async function shutdown() {
  await closeBrowser();
  try { fs.unlinkSync(SOCKET); } catch { /* already gone */ }
  process.exit(0);
}
process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);
