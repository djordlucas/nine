import type { JournalEvent } from './api/events';

/** What kind of thing an event is, for its badge. */
export type EventKind = 'turn' | 'llm' | 'tool' | 'agent' | 'process' | 'attention' | 'state' | 'other';

export function eventKind(type: string): EventKind {
  if (type.startsWith('turn_')) return 'turn';
  if (type.startsWith('llm_') || type === 'thinking' || type === 'context_update') return 'llm';
  if (type.startsWith('tool_')) return 'tool';
  if (type.startsWith('sub_agent_') || type === 'supervisor') return 'agent';
  if (type.startsWith('process_') || type.startsWith('standing_') || type.startsWith('plugin_job_')) return 'process';
  if (type === 'human_input_requested' || type === 'capability_requested') return 'attention';
  if (/^(human_input|capability|notification|goal|workflow)/.test(type)) return 'state';
  return 'other';
}

type Payload = Record<string, unknown>;

function str(v: unknown): string {
  return typeof v === 'string' ? v : v == null ? '' : String(v);
}

/** One line of text, at most n characters. */
export function clip(s: string, n = 120): string {
  const line = s.replace(/\s+/g, ' ').trim();
  return line.length > n ? line.slice(0, n - 1) + '…' : line;
}

function ms(v: unknown): string {
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return '';
  return n < 1000 ? `${n} ms` : `${(n / 1000).toFixed(1)} s`;
}

function join(...parts: string[]): string {
  return parts.filter(Boolean).join(' · ');
}

/** Whether the stream left this event's payload out for its size. */
export function omittedBytes(e: JournalEvent): number | null {
  const p = e.payload as Payload | undefined;
  return p && p.omitted === true && typeof p.bytes === 'number' ? p.bytes : null;
}

/** A one-line summary of what an event says. */
export function summarize(e: JournalEvent): string {
  const omitted = omittedBytes(e);
  if (omitted !== null) return `payload omitted (${Math.round(omitted / 1024)} KB)`;
  const p = (e.payload ?? {}) as Payload;
  switch (e.type) {
    case 'turn_start':
      return join(p.trigger && p.trigger !== 'user' ? str(p.trigger) : '', clip(str(p.input)));
    case 'turn_end':
      return p.error
        ? `failed: ${clip(str(p.error))}`
        : join(clip(str(p.result), 80), ms(p.duration_ms), p.tool_count ? `${str(p.tool_count)} tools` : '');
    case 'llm_request':
      return join(`call ${str(p.llm_call_n)}`, p.tokens_used ? `${str(p.tokens_used)}/${str(p.budget)} tokens` : '');
    case 'llm_response':
      return join(
        `call ${str(p.llm_call_n)}`,
        str(p.stop_reason),
        Array.isArray(p.tool_calls) && p.tool_calls.length ? `${p.tool_calls.length} tool calls` : '',
      );
    case 'tool_start':
      return str(p.name);
    case 'tool_end':
      return join(str(p.name), ms(p.duration_ms), p.error ? `failed: ${clip(str(p.error), 80)}` : '');
    case 'tool_http':
      return join(str(p.tool), `${str(p.method)} ${str(p.host)}`.trim(), p.status ? str(p.status) : str(p.error));
    case 'context_update':
      return `${str(p.tokens_used)}/${str(p.budget)} tokens`;
    case 'sub_agent_start':
    case 'sub_agent_end':
      return join(str(p.role), clip(str(p.task), 80), str(p.status));
    case 'human_input_requested':
      return clip(str(p.question));
    case 'human_input_answered':
      return clip(str(p.answer));
    case 'capability_requested':
      return join(str(p.tool), str(p.capability), clip(str(p.reason), 60));
    case 'capability_decided':
      return str(p.status);
    case 'capability_revoked':
      return str(p.grant_id);
    case 'notification':
      return clip(str(p.message));
    case 'goal_created':
      return clip(str(p.description));
    case 'goal_status':
      return str(p.status);
    case 'goal_deleted':
      return Array.isArray(p.ids) ? `${p.ids.length} goals` : '';
    case 'workflow_created':
      return join(str(p.name), p.steps ? `${str(p.steps)} steps` : '');
    case 'workflow_step':
      return join(str(p.step_id), str(p.status), str(p.workflow_status));
  }
  const keys = Object.keys(p);
  return keys.length ? clip(JSON.stringify(p)) : '';
}

/** A short form of an id: enough to tell sessions apart. */
export function shortId(id: string): string {
  return id.length > 12 ? id.slice(0, 8) : id;
}

/** The event's time of day, local. */
export function timeOf(e: JournalEvent): string {
  const d = new Date(e.timestamp);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}
