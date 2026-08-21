// http_get — fetch a URL and return its body.
//
// Replaces part of the `http` built-in plugin. The plugin was a subprocess with
// the daemon's uid and **none** of the SSRF checks — it would happily fetch
// http://169.254.169.254/ and hand the model a cloud instance's credentials.
//
// This declares net.http with a "*" host allowlist, because the tool exists to
// retrieve whatever URL the model chose and no host list expresses that. The
// wildcard grants any *host*; it does not grant any *address*. Every connection
// is checked at dial time, so loopback, link-local, private ranges and multicast
// stay blocked — including across redirects and DNS rebinding, which is where a
// check on the URL alone would have been fooled.
//
// So this is strictly less authority than the plugin it replaces, not more.

export default async function ({ url, headers }) {
  const res = await fetch(String(url), { headers: headers ?? {} });
  const body = await res.text();
  return { status: res.status, body };
}
