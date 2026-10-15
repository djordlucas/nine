import { describe, expect, it } from 'vitest';
import { fromConfig, goalTree, statusActions, type Goal } from './goals';

function goal(id: string, over: Partial<Goal> = {}): Goal {
  return { id, description: id, status: 'active', parent_type: 'operator', created_at: '2026-10-01T00:00:00Z', ...over };
}

describe('goalTree', () => {
  it('puts sub-goals under their parent, oldest first', () => {
    const rows = goalTree([
      goal('b', { parent_id: 'a', parent_type: 'goal', created_at: '2026-10-03T00:00:00Z' }),
      goal('a'),
      goal('c', { parent_id: 'a', parent_type: 'goal', created_at: '2026-10-02T00:00:00Z' }),
      goal('d', { parent_id: 'c', parent_type: 'goal' }),
    ]);
    expect(rows.map((r) => `${r.goal.id}${r.depth}`)).toEqual(['a0', 'c1', 'd2', 'b1']);
  });

  it('orders top-level goals active first, then newest', () => {
    const rows = goalTree([
      goal('old', { created_at: '2026-01-01T00:00:00Z' }),
      goal('done', { status: 'done', created_at: '2026-12-01T00:00:00Z' }),
      goal('new', { created_at: '2026-06-01T00:00:00Z' }),
    ]);
    expect(rows.map((r) => r.goal.id)).toEqual(['new', 'old', 'done']);
  });

  it('shows a goal whose parent is missing at the top level', () => {
    const rows = goalTree([goal('orphan', { parent_id: 'gone', parent_type: 'goal' })]);
    expect(rows).toEqual([{ goal: expect.objectContaining({ id: 'orphan' }), depth: 0 }]);
  });

  it('keeps a goal with a conversation parent at the top level', () => {
    const rows = goalTree([goal('x', { parent_id: 'conv-1', parent_type: 'conversation' })]);
    expect(rows[0].depth).toBe(0);
  });
});

describe('statusActions', () => {
  it('pauses an active goal and resumes a paused one', () => {
    expect(statusActions('active').map(([s]) => s)).toEqual(['paused', 'done', 'archived']);
    expect(statusActions('paused')[0]).toEqual(['active', 'Resume']);
    expect(statusActions('done')).toEqual([['active', 'Reactivate']]);
  });
});

describe('fromConfig', () => {
  it('marks a goal nine.toml declares', () => {
    expect(fromConfig(goal('s', { parent_type: 'config' }))).toBe(true);
    expect(fromConfig(goal('s'))).toBe(false);
  });
});
