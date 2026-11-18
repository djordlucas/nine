import { describe, expect, it } from 'vitest';
import { sectionFor, sections } from './sections';

describe('sectionFor', () => {
  it('matches a section and its sub-paths', () => {
    expect(sectionFor('/conversations').label).toBe('Agents');
    expect(sectionFor('/conversations/sec-watch').label).toBe('Agents');
  });
  it('does not match on a shared prefix alone', () => {
    expect(sectionFor('/builderx').label).toBe('Agents');
    expect(sectionFor('/toolsmith').label).toBe('Agents');
  });
  it('falls back to the first section', () => {
    expect(sectionFor('/').label).toBe('Agents');
    expect(sectionFor('/nowhere').label).toBe('Agents');
    // Process, goal and board deep links still land on the Agents section.
    expect(sectionFor('/processes/self-reflection').label).toBe('Agents');
    expect(sectionFor('/goals/some-goal').label).toBe('Agents');
    expect(sectionFor('/board/task_1').label).toBe('Agents');
  });
  it('matches the Builder section and its sub-paths', () => {
    expect(sectionFor('/builder').label).toBe('Builder');
    expect(sectionFor('/builder/skills/code-review').label).toBe('Builder');
  });
  it('lists every section for the mobile tab bar, foot ones included', () => {
    expect(sections.map((s) => s.label)).toEqual(['Agents', 'Builder', 'Inbox', 'Data', 'Journal', 'System']);
  });
});
