import { api, get, type Schemas } from './api/client';

export type CapabilityRequest = Schemas['CapabilityRequest'];
export type Grant = Schemas['CapabilityGrant'];
export type Question = Schemas['Question'];

/** The events after which what waits for the operator may have changed. */
export const attentionEvents = ['capability_*', 'human_input_*'];

/** What waits for the operator: pending capability requests and unanswered questions. */
export async function loadAttention(): Promise<{ requests: CapabilityRequest[]; questions: Question[]; grants: Grant[] }> {
  const [caps, qs] = await Promise.all([
    get(api.GET('/capabilities'), 'capability requests'),
    get(api.GET('/questions'), 'questions'),
  ]);
  return {
    requests: (caps.requests ?? []).filter((r) => r.status === 'pending'),
    questions: qs.data,
    grants: caps.grants ?? [],
  };
}

/**
 * One value of a scope as text: a mount as host=>guest, as the Tools view
 * shows it; another object as JSON.
 */
function item(x: unknown): string {
  if (x && typeof x === 'object') {
    const m = x as Record<string, unknown>;
    if (typeof m.host === 'string' && typeof m.guest === 'string') return `${m.host}=>${m.guest}`;
    return JSON.stringify(x);
  }
  return String(x);
}

/** A capability request's scope as text: its params, or "any" when it named none. */
export function scopeOf(params: string | undefined): string {
  if (!params) return 'any';
  try {
    const v: unknown = JSON.parse(params);
    if (v && typeof v === 'object' && !Array.isArray(v)) {
      const parts = Object.entries(v as Record<string, unknown>).map(
        ([k, x]) => `${k}: ${Array.isArray(x) ? x.map(item).join(', ') : item(x)}`,
      );
      return parts.length ? parts.join('; ') : 'any';
    }
    return String(v);
  } catch {
    return params;
  }
}
