import { describe, expect, it } from 'vitest';
import { bytes, docLink } from './system';

const topics = { docs: ['processes', 'api', 'usage'], spec: ['api', 'processes', 'conformance'] };

describe('docLink', () => {
  it('opens a sibling topic on the same kind of page', () => {
    expect(docLink('docs', 'usage.md', topics)).toBe('/system/docs/usage');
    expect(docLink('spec', 'conformance.md#r-proc', topics)).toBe('/system/spec/conformance#r-proc');
  });

  it('follows the folder a link names across docs and spec', () => {
    expect(docLink('docs', '../spec/contracts/api.md', topics)).toBe('/system/spec/api');
    expect(docLink('spec', '../../docs/processes.md#goals', topics)).toBe('/system/docs/processes#goals');
    expect(docLink('spec', 'contracts/processes.md', topics)).toBe('/system/spec/processes');
  });

  it('falls back to the other kind for a bare name only one has', () => {
    expect(docLink('spec', 'usage.md', topics)).toBe('/system/docs/usage');
  });

  it('gives null for a Markdown file the build does not embed', () => {
    expect(docLink('docs', '../adr/web-ui.md', topics)).toBeNull();
    expect(docLink('docs', 'missing.md', topics)).toBeNull();
  });

  it('leaves other links alone', () => {
    expect(docLink('docs', 'https://example.com/x.md', topics)).toBe('https://example.com/x.md');
    expect(docLink('docs', '#section', topics)).toBe('#section');
    expect(docLink('docs', 'image.png', topics)).toBe('image.png');
  });
});

describe('bytes', () => {
  it('sizes briefly', () => {
    expect(bytes(512)).toBe('512 B');
    expect(bytes(2048)).toBe('2.0 KiB');
    expect(bytes(5 * 1024 * 1024)).toBe('5.0 MiB');
  });
});
