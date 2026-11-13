import { describe, expect, it } from 'vitest';
import { sectionFor, sections } from './sections';

describe('sectionFor', () => {
  it('matches a section and its sub-paths', () => {
    expect(sectionFor('/conversations').label).toBe('Agents');
    expect(sectionFor('/conversations/sec-watch').label).toBe('Agents');
  });
  it('does not match on a shared prefix alone', () => {
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
  it('gives the mobile tab bar two sections, leaving room for More', () => {
    expect(sections.filter((s) => s.tab)).toHaveLength(2);
  });
});
