// http_post — POST a body to a URL and return the response.
//
// See http_get.js for why the host allowlist is "*" and why that is safe.

export default async function ({ url, body, headers }) {
  const res = await fetch(String(url), {
    method: "POST",
    headers: { "Content-Type": "application/json", ...(headers ?? {}) },
    body: typeof body === "string" ? body : JSON.stringify(body ?? {}),
  });
  const text = await res.text();
  return { status: res.status, body: text };
}
