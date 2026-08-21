// web_search — search the web and return titles, URLs, and snippets.
//
// Replaces the last of the `http` built-in plugin. Unlike the fetching tools,
// this one has a *real* host allowlist: it talks to three known search
// endpoints and nothing else, so its grant names them rather than "*". That is
// the capability model working as intended — the tool that can be constrained
// is constrained.
//
// DuckDuckGo's HTML endpoint is scraped, which is why nine:html exists: the
// results are elements carrying known classes, which a tokenizer can pull out
// without needing a DOM.

import { findByClass } from "nine:html";
import { get as env } from "nine:env";

const DDG = "https://html.duckduckgo.com/html/?q=";
const BRAVE = "https://api.search.brave.com/res/v1/web/search";
const SERPAPI = "https://serpapi.com/search";

/** DDG wraps results in a redirector; the real URL is the uddg parameter. */
function decodeDDGURL(href) {
  if (!href.includes("uddg=")) return href;
  const abs = href.startsWith("//") ? "https:" + href : href;
  try {
    const u = new URL(abs, "https://duckduckgo.com");
    return u.searchParams.get("uddg") ?? href;
  } catch {
    return href;
  }
}

async function duckduckgo(query, limit) {
  const res = await fetch(DDG + encodeURIComponent(query), {
    headers: {
      "User-Agent": "Mozilla/5.0 (compatible; nine-agent/1.0)",
      "Accept-Language": "en-US,en;q=0.9",
    },
  });
  const html = await res.text();
  // Titles and snippets appear in document order; zip them, as the plugin did.
  const titles = findByClass(html, "result__a");
  const snippets = findByClass(html, "result__snippet");
  const out = [];
  for (let i = 0; i < titles.length && i < snippets.length && out.length < limit; i++) {
    out.push({
      title: titles[i].text,
      url: decodeDDGURL(titles[i].attrs.href ?? ""),
      snippet: snippets[i].text,
    });
  }
  return out;
}

async function brave(query, limit, apiKey) {
  const res = await fetch(`${BRAVE}?q=${encodeURIComponent(query)}&count=${limit}`, {
    headers: { Accept: "application/json", "X-Subscription-Token": apiKey },
  });
  const body = await res.json();
  return (body?.web?.results ?? []).slice(0, limit).map((r) => ({
    title: r.title ?? "",
    url: r.url ?? "",
    snippet: r.description ?? "",
  }));
}

async function serpapi(query, limit, apiKey) {
  const res = await fetch(
    `${SERPAPI}?q=${encodeURIComponent(query)}&num=${limit}&api_key=${encodeURIComponent(apiKey)}`,
  );
  const body = await res.json();
  return (body?.organic_results ?? []).slice(0, limit).map((r) => ({
    title: r.title ?? "",
    url: r.link ?? "",
    snippet: r.snippet ?? "",
  }));
}

export default async function ({ query, limit }) {
  const n = Number.isFinite(limit) && limit > 0 ? Math.min(limit, 25) : 10;
  const provider = env("SEARCH_PROVIDER") ?? "";
  const apiKey = env("SEARCH_API_KEY") ?? "";

  switch (provider) {
    case "brave":
      return await brave(String(query), n, apiKey);
    case "serpapi":
      return await serpapi(String(query), n, apiKey);
    default:
      return await duckduckgo(String(query), n);
  }
}
