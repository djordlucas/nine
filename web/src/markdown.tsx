import { h, type ComponentChildren } from 'preact';
import { Lexer, type Token, type Tokens } from 'marked';

/**
 * Markdown from the model, rendered as Preact elements (adr/web-ui.md §7).
 * marked only lexes; the tokens become elements here, so nothing the model
 * writes is ever parsed as HTML: raw HTML in a reply shows as text, a link
 * goes only to http(s) or mailto, and an image is a link, since the page loads
 * nothing from another origin.
 */
export function Markdown({
  text,
  class: cls,
  link,
}: {
  text: string;
  class?: string;
  /** Rewrites each link and image target first; null makes it plain text. */
  link?: (href: string) => string | null;
}) {
  let tokens: Token[];
  try {
    tokens = Lexer.lex(text, { gfm: true, breaks: false });
  } catch {
    return <p class={cls}>{text}</p>;
  }
  if (link) rewriteLinks(tokens, link);
  return <div class={cls}>{blocks(tokens)}</div>;
}

// A target safeHref refuses, so a link the rewrite drops renders as its text.
const dropped = 'about:blank';

function rewriteLinks(tokens: Token[], link: (href: string) => string | null) {
  for (const t of tokens) {
    if (t.type === 'link' || t.type === 'image') {
      const l = t as Tokens.Link | Tokens.Image;
      l.href = link(l.href) ?? dropped;
    }
    const nested = t as { tokens?: Token[]; items?: Tokens.ListItem[] };
    if (nested.tokens) rewriteLinks(nested.tokens, link);
    if (nested.items) for (const it of nested.items) rewriteLinks(it.tokens, link);
    if (t.type === 'table') {
      const tb = t as Tokens.Table;
      for (const c of tb.header) rewriteLinks(c.tokens, link);
      for (const row of tb.rows) for (const c of row) rewriteLinks(c.tokens, link);
    }
  }
}

/** href as a path when it stays on this site, where the router opens it in place; else null. */
function onSite(href: string): string | null {
  try {
    const u = new URL(href);
    return u.origin === location.origin ? u.pathname + u.search + u.hash : null;
  } catch {
    return null;
  }
}

/** Whether a link may be followed: http(s) and mailto only. */
export function safeHref(href: string): string | null {
  try {
    const u = new URL(href, location.origin);
    return ['http:', 'https:', 'mailto:'].includes(u.protocol) ? u.href : null;
  } catch {
    return null;
  }
}

function blocks(tokens: Token[]): ComponentChildren[] {
  return tokens.map((t, i) => block(t, i));
}

function block(t: Token, key: number): ComponentChildren {
  switch (t.type) {
    case 'space':
    case 'def':
      return null;
    case 'paragraph':
      return <p key={key}>{inline((t as Tokens.Paragraph).tokens)}</p>;
    case 'heading': {
      const hd = t as Tokens.Heading;
      // Headings in a reply are part of a message, not of the page outline.
      return h(`h${Math.min(6, hd.depth + 2)}`, { key }, inline(hd.tokens));
    }
    case 'code': {
      const c = t as Tokens.Code;
      return (
        <pre key={key} data-lang={c.lang || undefined}>
          <code>{c.text}</code>
        </pre>
      );
    }
    case 'blockquote':
      return <blockquote key={key}>{blocks((t as Tokens.Blockquote).tokens)}</blockquote>;
    case 'list': {
      const l = t as Tokens.List;
      const items = l.items.map((it, j) => (
        <li key={j}>
          {it.task && <input type="checkbox" checked={it.checked} disabled />}
          {it.loose ? blocks(it.tokens) : listItem(it.tokens)}
        </li>
      ));
      return l.ordered ? (
        <ol key={key} start={l.start === '' || l.start === 1 ? undefined : l.start}>
          {items}
        </ol>
      ) : (
        <ul key={key}>{items}</ul>
      );
    }
    case 'table': {
      const tb = t as Tokens.Table;
      return (
        <div key={key} class="md-table">
          <table>
            <thead>
              <tr>
                {tb.header.map((c, j) => (
                  <th key={j}>{inline(c.tokens)}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {tb.rows.map((r, j) => (
                <tr key={j}>
                  {r.map((c, k) => (
                    <td key={k}>{inline(c.tokens)}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
    }
    case 'hr':
      return <hr key={key} />;
    case 'html':
      return <p key={key}>{(t as Tokens.HTML).text}</p>;
    case 'text': {
      const tx = t as Tokens.Text;
      return <p key={key}>{tx.tokens ? inline(tx.tokens) : tx.text}</p>;
    }
  }
  return 'raw' in t ? <p key={key}>{String(t.raw)}</p> : null;
}

/** A tight list item's tokens: its text runs inline, other blocks as blocks. */
function listItem(tokens: Token[]): ComponentChildren[] {
  return tokens.map((t, i) =>
    t.type === 'text' ? <span key={i}>{(t as Tokens.Text).tokens ? inline((t as Tokens.Text).tokens!) : (t as Tokens.Text).text}</span> : block(t, i),
  );
}

function inline(tokens: Token[]): ComponentChildren[] {
  return tokens.map((t, i) => inlineToken(t, i));
}

function inlineToken(t: Token, key: number): ComponentChildren {
  switch (t.type) {
    case 'text': {
      const tx = t as Tokens.Text;
      return tx.tokens ? <span key={key}>{inline(tx.tokens)}</span> : tx.text;
    }
    case 'escape':
      return (t as Tokens.Escape).text;
    case 'strong':
      return <strong key={key}>{inline((t as Tokens.Strong).tokens)}</strong>;
    case 'em':
      return <em key={key}>{inline((t as Tokens.Em).tokens)}</em>;
    case 'del':
      return <del key={key}>{inline((t as Tokens.Del).tokens)}</del>;
    case 'codespan':
      return <code key={key}>{(t as Tokens.Codespan).text}</code>;
    case 'br':
      return <br key={key} />;
    case 'link': {
      const l = t as Tokens.Link;
      const href = safeHref(l.href);
      if (!href) return <span key={key}>{inline(l.tokens)}</span>;
      const path = onSite(href);
      if (path) {
        return (
          <a key={key} href={path}>
            {inline(l.tokens)}
          </a>
        );
      }
      return (
        <a key={key} href={href} target="_blank" rel="noopener noreferrer">
          {inline(l.tokens)}
        </a>
      );
    }
    case 'image': {
      const im = t as Tokens.Image;
      const href = safeHref(im.href);
      const label = im.text || im.href;
      return href ? (
        <a key={key} href={href} target="_blank" rel="noopener noreferrer">
          {label}
        </a>
      ) : (
        label
      );
    }
    case 'html':
      return (t as Tokens.HTML).text;
  }
  return 'raw' in t ? String(t.raw) : null;
}
