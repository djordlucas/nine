/** Where a topic of the embedded documentation opens in the UI. */
export type DocKind = 'docs' | 'spec';

export function topicPath(kind: DocKind, topic: string): string {
  return `/system/${kind}/${encodeURIComponent(topic)}`;
}

/**
 * Maps a link inside an embedded docs or spec topic to the page that shows its
 * target. Topics are named by file name, so `processes.md`,
 * `../docs/processes.md#goals` and `contracts/api.md` all resolve by their last
 * segment, to docs or spec by the folder they name or, failing that, the page
 * they are on. A link to a Markdown file the build does not embed (an ADR, a
 * README) resolves to null. Any other link is returned as it is.
 */
export function docLink(from: DocKind, href: string, topics: { docs: readonly string[]; spec: readonly string[] }): string | null {
  if (/^[a-z][a-z0-9+.-]*:/i.test(href) || href.startsWith('//')) return href;
  if (href.startsWith('#')) return href;
  const [path, frag] = splitFragment(href);
  if (!path.toLowerCase().endsWith('.md')) return href;
  const segments = path.split('/').filter((s) => s && s !== '.');
  const topic = segments[segments.length - 1].slice(0, -3);
  const folders = segments.slice(0, -1);
  let kind: DocKind = from;
  if (folders.includes('spec') || folders.includes('contracts')) kind = 'spec';
  else if (folders.includes('docs')) kind = 'docs';
  else if (folders.some((f) => f !== '..')) return null;
  if (!topics[kind].includes(topic)) {
    const other: DocKind = kind === 'docs' ? 'spec' : 'docs';
    if (folders.length === 0 && topics[other].includes(topic)) kind = other;
    else return null;
  }
  return topicPath(kind, topic) + (frag ? `#${frag}` : '');
}

function splitFragment(href: string): [string, string] {
  const i = href.indexOf('#');
  return i < 0 ? [href, ''] : [href.slice(0, i), href.slice(i + 1)];
}

/** A size in bytes, briefly. */
export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let v = n / 1024;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v.toFixed(1)} ${units[u]}`;
}
