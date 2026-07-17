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
];

function isPrivateHost(hostname) {
  return PRIVATE_RANGES.some(r => r.test(hostname));
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
