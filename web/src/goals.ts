import type { Schemas } from './api/client';

export type Goal = Schemas['GoalInfo'];
export type GoalStatus = Schemas['SetGoalStatusRequest']['status'];

/** A goal and how deep it sits under its top-level goal. */
export interface GoalRow {
  goal: Goal;
  depth: number;
}

const statusOrder: Record<string, number> = { active: 0, paused: 1, done: 2, archived: 3 };

/**
 * Goals in tree order: top-level goals, active first then newest, each
 * followed by its sub-goals, oldest first. A goal whose parent is not in the
 * list is shown at the top level.
 */
export function goalTree(goals: readonly Goal[]): GoalRow[] {
  const ids = new Set(goals.map((g) => g.id));
  const children = new Map<string, Goal[]>();
  const roots: Goal[] = [];
  for (const g of goals) {
    if (g.parent_type === 'goal' && g.parent_id && ids.has(g.parent_id)) {
      const list = children.get(g.parent_id) ?? [];
      list.push(g);
      children.set(g.parent_id, list);
    } else {
      roots.push(g);
    }
  }
  roots.sort(
    (a, b) =>
      (statusOrder[a.status ?? ''] ?? 9) - (statusOrder[b.status ?? ''] ?? 9) ||
      (b.created_at ?? '').localeCompare(a.created_at ?? ''),
  );
  const out: GoalRow[] = [];
  const seen = new Set<string>();
  const walk = (g: Goal, depth: number) => {
    if (!g.id || seen.has(g.id)) return;
    seen.add(g.id);
    out.push({ goal: g, depth });
    const kids = [...(children.get(g.id) ?? [])].sort((a, b) => (a.created_at ?? '').localeCompare(b.created_at ?? ''));
    for (const k of kids) walk(k, depth + 1);
  };
  for (const r of roots) walk(r, 0);
  return out;
}

/** The status changes a goal offers, as [status, label]. */
export function statusActions(status: string | undefined): [GoalStatus, string][] {
  switch (status) {
    case 'active':
      return [
        ['paused', 'Pause'],
        ['done', 'Mark done'],
        ['archived', 'Archive'],
      ];
    case 'paused':
      return [
        ['active', 'Resume'],
        ['done', 'Mark done'],
        ['archived', 'Archive'],
      ];
    default:
      return [['active', 'Reactivate']];
  }
}

/** A goal nine.toml declares: the next boot would re-create it, so it cannot be deleted here. */
export function fromConfig(g: Goal): boolean {
  return g.parent_type === 'config';
}
