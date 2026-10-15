import type { Schemas } from './api/client';

export type Process = Schemas['ProcessInfo'];

/** A process's state as the UI shows it: one its budget stopped reads as paused. */
export type ProcessState = 'running' | 'paused' | 'failing' | 'stopped';

export function processState(p: Process): ProcessState {
  if (p.state === 'stopped' && p.stopped_by === 'budget') return 'paused';
  return p.state;
}

const stateOrder: Record<ProcessState, number> = { failing: 0, paused: 1, running: 2, stopped: 3 };

/** Processes that need a look first: failing, then paused, running, stopped; by id within a state. */
export function byAttention(list: readonly Process[]): Process[] {
  return [...list].sort(
    (a, b) => stateOrder[processState(a)] - stateOrder[processState(b)] || a.id.localeCompare(b.id),
  );
}

/** What the operator can do to a process (docs/processes.md). */
export interface ProcessActions {
  start: boolean;
  stop: boolean;
  /** The goal to set active: its status, not a start, decides whether the process runs. */
  resumeGoal?: string;
  /** A live process takes the operator's messages while it runs. */
  send: boolean;
}

export function actionsFor(p: Process): ProcessActions {
  const running = p.state === 'running' || p.state === 'failing';
  const goalStopped = p.state === 'stopped' && p.stopped_by === 'goal' && !!p.goal;
  return {
    // A failing process can be started too: that retries it now.
    start: p.state === 'failing' || (p.state === 'stopped' && !goalStopped),
    stop: running,
    resumeGoal: goalStopped ? p.goal : undefined,
    send: p.mode === 'live' && p.state === 'running',
  };
}

/** Why a stopped process is stopped, in a sentence. */
export function stoppedReason(p: Process): string {
  if (p.state !== 'stopped') return '';
  switch (p.stopped_by) {
    case 'operator':
      return 'Stopped by the operator.';
    case 'model':
      return 'Stopped by the model.';
    case 'goal':
      return 'Stopped by its goal, which is no longer active. Resuming the goal starts it again.';
    case 'budget':
      return "Paused by its budget. It runs again when its day ends; starting it now resets the day's usage.";
    case 'self':
      return 'It stopped itself.';
  }
  return 'Stopped.';
}

/** Used of a daily budget, as a fraction from 0 to 1; 0 when there is no limit. */
export function used(n: number, perDay: number): number {
  if (perDay <= 0) return 0;
  return Math.min(1, Math.max(0, n / perDay));
}
