// web_page_read — fetch a page and return it as readable text.
//
// The extraction is nine:html, a tokenizer rather than a tree builder. That is
// enough here: flattening a page needs to know where tags begin and end, not how
// a browser would repair mis-nesting. Script and style contents are dropped, and
// "<" inside them is not treated as markup — the case a naive stripper gets
// wrong and swallows the page for.
//
// Host allowlist is "*" for the same reason as http_get, and safe for the same
// reason: the address checks run at dial time regardless.

import { textOf } from "nine:html";

export default async function ({ url }) {
  const res = await fetch(String(url), {
    headers: { "User-Agent": "Mozilla/5.0 (compatible; nine-agent/1.0)" },
  });
  const html = await res.text();
  return { status: res.status, text: textOf(html) };
}
