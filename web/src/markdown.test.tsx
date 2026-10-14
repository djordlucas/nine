import { render } from '@testing-library/preact';
import { describe, expect, it } from 'vitest';
import { Markdown, safeHref } from './markdown';

const html = (text: string) => render(<Markdown text={text} />).container.innerHTML;

describe('markdown', () => {
  it('renders the common blocks', () => {
    const out = html('# Title\n\nSome **bold** and *em* and `code`.\n\n- a\n- b\n\n1. one\n\n```js\nlet x = 1;\n```\n\n> quoted');
    expect(out).toContain('<h3>Title</h3>');
    expect(out).toContain('<strong>bold</strong>');
    expect(out).toContain('<em>em</em>');
    expect(out).toContain('<code>code</code>');
    expect(out).toContain('<li><span>a</span></li>');
    expect(out).toContain('<ol>');
    expect(out).toContain('<pre data-lang="js"><code>let x = 1;</code></pre>');
    expect(out).toContain('<blockquote>');
  });

  it('never turns the model’s HTML into elements', () => {
    const out = html('<img src=x onerror=alert(1)> and <b>bold</b>\n\n<script>alert(1)</script>');
    expect(out).not.toContain('<img');
    expect(out).not.toContain('<script');
    expect(out).not.toContain('<b>');
    expect(out).toContain('&lt;script&gt;');
  });

  it('links only to http(s) and mailto, in a new tab', () => {
    const out = html('[ok](https://example.com) [bad](javascript:alert(1)) ![pic](https://example.com/p.png)');
    expect(out).toContain('<a href="https://example.com/" target="_blank" rel="noopener noreferrer">ok</a>');
    expect(out).not.toContain('javascript:');
    expect(out).toContain('>pic</a>');
    expect(safeHref('mailto:a@b.c')).toBe('mailto:a@b.c');
    expect(safeHref('data:text/html,x')).toBeNull();
  });

  it('renders a table', () => {
    const out = html('| a | b |\n|---|---|\n| 1 | 2 |');
    expect(out).toContain('<th>a</th>');
    expect(out).toContain('<td>2</td>');
  });

  it('copes with half a reply, as it streams', () => {
    expect(() => html('Some **bol')).not.toThrow();
    expect(html('```\nunfinished')).toContain('unfinished');
  });
});
