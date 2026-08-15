import { minimatch } from 'minimatch';
import { config } from './config.js';

function blockError(msg) {
  const err = new Error(msg);
  err.__rpc = true;
  err.code = -32603;
  return err;
}

const PRIVATE_RANGES = [
  /^localhost$/i,
  /^127\./,
  /^10\./,
  /^172\.(1[6-9]|2\d|3[01])\./,
  /^192\.168\./,
  /^::1$/,
  /^fc00:/i,
  /^fe80:/i,

  // Link-local, which is the SSRF target that actually matters: 169.254.169.254
  // is the cloud instance-metadata endpoint on AWS, GCP and Azure, and it serves
  // credentials to anything that asks. An agent following a link, or a page's
  // own instruction, is exactly the "anything" this is guarding. The whole /16
  // is covered rather than the one address, and the vendor hostnames alias it.
  /^169\.254\./,
  /^metadata\.google\.internal$/i,
  /^metadata$/i,
  /^fe8[0-9a-f]:/i,          // fe80::/10 is wider than the fe80: prefix above
  /^f[cd][0-9a-f]{2}:/i,     // fc00::/7 — unique local, both halves

  // 0.0.0.0 and its IPv6 form route to the local host on Linux.
  /^0\.0\.0\.0$/,
  /^::$/,
];

function isPrivateHost(hostname) {
  // WHATWG URL keeps the brackets on an IPv6 host: new URL('http://[::1]/')
  // has hostname '[::1]', so every IPv6 pattern below silently never matched
  // and IPv6 private addresses were reachable. The repo's own '::1' test was
  // already failing — bun tests are not part of `make test`, so nothing said so.
  const host = hostname.startsWith('[') && hostname.endsWith(']')
    ? hostname.slice(1, -1)
    : hostname;
  return PRIVATE_RANGES.some(r => r.test(host));
}

export function checkUrl(url) {
  let parsed;
  try {
    parsed = new URL(url);
  } catch {
    const err = new Error(`invalid URL: ${url}`);
    err.__rpc = true;
    err.code = -32602;
    throw err;
  }

  if (isPrivateHost(parsed.hostname) && !config.allowPrivate) {
    throw blockError(`blocked: private host ${parsed.hostname}`);
  }

  if (config.allowUrls.length > 0) {
    if (!config.allowUrls.some(p => minimatch(url, p)))
      throw blockError(`blocked: URL not in allowlist: ${url}`);
  }

  if (config.blockUrls.some(p => minimatch(url, p)))
    throw blockError(`blocked: URL matches blocklist: ${url}`);
}
