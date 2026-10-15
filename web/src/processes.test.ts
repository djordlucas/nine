import { describe, expect, it } from 'vitest';
import { actionsFor, byAttention, processState, stoppedReason, used, type Process } from './processes';

const budget = { turns: 0, turns_per_day: 100, tokens: 0, tokens_per_day: 1000 };

function proc(over: Partial<Process>): Process {
  return { id: 'p', tool: 't', mode: 'live', state: 'running', trigger: '—', budget, ...over };
}

describe('processState', () => {
  it('reads a budget stop as paused', () => {
    expect(processState(proc({ state: 'stopped', stopped_by: 'budget' }))).toBe('paused');
    expect(processState(proc({ state: 'stopped', stopped_by: 'operator' }))).toBe('stopped');
    expect(processState(proc({ state: 'failing' }))).toBe('failing');
  });
});

describe('byAttention', () => {
  it('puts failing, then paused, first', () => {
    const list = [
      proc({ id: 'b', state: 'stopped' }),
      proc({ id: 'a', state: 'running' }),
      proc({ id: 'c', state: 'stopped', stopped_by: 'budget' }),
      proc({ id: 'd', state: 'failing' }),
    ];
    expect(byAttention(list).map((p) => p.id)).toEqual(['d', 'c', 'a', 'b']);
  });
});

describe('actionsFor', () => {
  it('offers stop and send for a running live process', () => {
    expect(actionsFor(proc({}))).toEqual({ start: false, stop: true, resumeGoal: undefined, send: true });
  });

  it('offers a retry and a stop for a failing one', () => {
    const a = actionsFor(proc({ state: 'failing' }));
    expect(a.start && a.stop && !a.send).toBe(true);
  });

  it('sends a goal-stopped process to its goal instead of a start', () => {
    const a = actionsFor(proc({ state: 'stopped', stopped_by: 'goal', goal: 'g1' }));
    expect(a).toEqual({ start: false, stop: false, resumeGoal: 'g1', send: false });
  });

  it('starts any other stopped process', () => {
    expect(actionsFor(proc({ state: 'stopped', stopped_by: 'operator' })).start).toBe(true);
    expect(actionsFor(proc({ state: 'stopped', stopped_by: 'budget' })).start).toBe(true);
  });

  it('sends no message to a slice process', () => {
    expect(actionsFor(proc({ mode: 'slice' })).send).toBe(false);
  });
});

describe('stoppedReason', () => {
  it('says nothing for a running process and why for a stopped one', () => {
    expect(stoppedReason(proc({}))).toBe('');
    expect(stoppedReason(proc({ state: 'stopped', stopped_by: 'goal' }))).toMatch(/goal/);
  });
});

describe('used', () => {
  it('is a clamped fraction, and 0 with no limit', () => {
    expect(used(50, 100)).toBe(0.5);
    expect(used(150, 100)).toBe(1);
    expect(used(5, 0)).toBe(0);
  });
});
