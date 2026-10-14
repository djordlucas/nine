import { describe, expect, it } from 'vitest';
import { sectionFor, sections } from './sections';

describe('sectionFor', () => {
  it('matches a section and its sub-paths', () => {
    expect(sectionFor('/processes').label).toBe('Processes');
    expect(sectionFor('/processes/sec-watch').label).toBe('Processes');
  });
  it('does not match on a shared prefix alone', () => {
    expect(sectionFor('/toolsmith').label).toBe('Dashboard');
  });
  it('falls back to the dashboard', () => {
    expect(sectionFor('/').label).toBe('Dashboard');
    expect(sectionFor('/nowhere').label).toBe('Dashboard');
  });
  it('gives the mobile tab bar four sections, leaving room for More', () => {
    expect(sections.filter((s) => s.tab)).toHaveLength(4);
  });
});
