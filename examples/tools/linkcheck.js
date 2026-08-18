// linkcheck — read a list of URLs from a mounted file, fetch each, report what
// came back.
//
// This is the worked example for the `js` kind, and it deliberately touches
// everything a real tool needs: a granted filesystem, a granted network, URL
// handling, concurrency, and failures that tell the model what to do about them.

import { readFileText, exists } from "nine:fs";

/**
 * Turn a model-supplied filename into a path inside the mount.
 *
 * Worth doing in any tool that takes a filename: a model will cheerfully pass
 * "urls.txt", "data/urls.txt", or "/data/urls.txt" for the same file, having read
 * the mount path in your description. Accepting all three costs three lines and
 * removes a whole class of "no such file" round trips.
 *
 * `..` is refused rather than normalized — the mount already makes escaping
 * impossible, so a path containing it is a mistake worth naming.
 */
function resolveInMount(name) {
  let n = String(name).trim().replace(/^\/+/, "");
  if (n.startsWith("data/")) n = n.slice("data/".length);
  if (n.split("/").includes("..")) {
    const e = new Error(`filename must not contain "..": ${name}`);
    e.code = "E_ARGS";
    e.retryable = false;
    throw e;
  }
  return `/data/${n}`;
}

/** Pull the URLs out of a text file: one per line, `#` comments and blanks skipped. */
function parseList(text) {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line && !line.startsWith("#"));
}

/**
 * Check one URL. Every failure here is *expected* — an unreachable host is a
 * result, not a bug — so this returns a row rather than throwing. Throwing is
 * reserved for "this call cannot work", which is a different thing entirely.
 */
async function check(raw) {
  let url;
  try {
    url = new URL(raw);
  } catch {
    return { url: raw, ok: false, error: "not a URL" };
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    return { url: raw, ok: false, error: `unsupported scheme ${url.protocol}` };
  }

  const started = Date.now();
  try {
    const res = await fetch(url.href, { method: "GET" });
    return {
      url: url.href,
      ok: res.ok,
      status: res.status,
      contentType: res.headers["content-type"] ?? "",
      ms: Date.now() - started,
    };
  } catch (e) {
    // A blocked request throws rather than returning a non-ok response, so this
    // catches both "the operator did not allow this host" and "the host is down".
    return { url: url.href, ok: false, error: e.message };
  }
}

export default async ({ file, urls }) => {
  let list = [];

  if (Array.isArray(urls) && urls.length > 0) {
    list = urls;
  } else if (file) {
    const path = resolveInMount(file);
    if (!exists(path)) {
      // A bad argument cannot become good on the second attempt, and saying so
      // takes this call out of Nine's retry loop instead of burning three tries.
      const e = new Error(`no such file: ${file}`);
      e.code = "E_ARGS";
      e.retryable = false;
      throw e;
    }
    list = parseList(readFileText(path));
  } else {
    const e = new Error("pass either urls (a list) or file (a filename in the data directory)");
    e.code = "E_ARGS";
    e.retryable = false;
    throw e;
  }

  if (list.length === 0) {
    const e = new Error("no URLs to check");
    e.code = "E_EMPTY";
    e.retryable = false;
    throw e;
  }

  // Concurrent because the deadline is wall-clock: five sequential requests to a
  // slow host would spend it, five parallel ones spend one request's worth.
  const rows = await Promise.all(list.map(check));

  const reachable = rows.filter((r) => r.ok).length;
  if (reachable === 0 && rows.every((r) => r.error)) {
    // Nothing was reachable and every failure was transport-shaped, so this may
    // well work later — which is the opposite instruction from the one above.
    const e = new Error(`none of the ${rows.length} URLs could be reached`);
    e.code = "E_UPSTREAM";
    e.retryable = true;
    e.cause = new Error(rows[0].error);
    throw e;
  }

  return { checked: rows.length, reachable, results: rows };
};
