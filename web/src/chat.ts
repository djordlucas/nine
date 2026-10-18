import type { Schemas } from './api/client';

/**
 * A conversation's timeline (adr/web-ui.md §5): what the transcript says
 * happened, then what the WebSocket reports as it happens. Pure functions over
 * plain data, so the chat view only renders.
 */

export type Item =
  | { kind: 'user'; text: string; turn?: number; pending?: boolean }
  | { kind: 'tool'; name: string; input?: unknown; output?: string; done: boolean }
  | { kind: 'agent'; id: string; task?: string; role?: string; status?: string }
  | { kind: 'reply'; text: string; turn?: number; streaming: boolean }
  | { kind: 'question'; id: string; question: string; options: string[]; answered?: string }
  | { kind: 'notice'; text: string }
  | { kind: 'error'; text: string; cancelled: boolean };

export interface Chat {
  items: Item[];
  /** A turn is running: one this tab sent, or one the stream shows. */
  busy: boolean;
}

export const empty: Chat = { items: [], busy: false };

/** The timeline from a conversation's transcript. */
export function fromHistory(entries: Schemas['HistoryEntry'][]): Item[] {
  const items: Item[] = [];
  for (const e of entries) {
    switch (e.type) {
      case 'user_turn':
        items.push({ kind: 'user', text: e.text ?? '', turn: e.turn_number });
        break;
      case 'response':
        items.push({ kind: 'reply', text: e.text ?? '', turn: e.turn_number, streaming: false });
        break;
      case 'tool_start':
        items.push({ kind: 'tool', name: e.tool_name ?? '', input: e.tool_input, done: false });
        break;
      case 'tool_end': {
        const open = lastOpenTool(items, e.tool_name ?? '');
        if (open) Object.assign(open, { output: e.tool_output, done: true });
        else items.push({ kind: 'tool', name: e.tool_name ?? '', input: e.tool_input, output: e.tool_output, done: true });
        break;
      }
      case 'sub_agent_start':
        items.push({ kind: 'agent', id: e.sub_agent_id ?? '', task: e.text, role: e.role });
        break;
      case 'turn_error':
        items.push(errorItem(e.text ?? ''));
        break;
      case 'sub_agent_end': {
        const a = items.findLast((i): i is Extract<Item, { kind: 'agent' }> => i.kind === 'agent' && i.id === e.sub_agent_id);
        if (a) a.status = e.status ?? 'done';
        break;
      }
    }
  }
  // A tool the transcript shows starting but not ending is one the turn was
  // cut off in; it is not running now.
  for (const i of items) if (i.kind === 'tool' && !i.done) i.done = true;
  return items;
}

/** A turn's error as the timeline shows it, live or from the transcript. */
function errorItem(message: string): Item {
  const cancelled = /turn cancelled/.test(message);
  return { kind: 'error', text: cancelled ? 'Cancelled.' : message || 'Something went wrong.', cancelled };
}

function lastOpenTool(items: Item[], name: string) {
  return items.findLast((i): i is Extract<Item, { kind: 'tool' }> => i.kind === 'tool' && !i.done && i.name === name);
}

/** The streaming reply of the running turn, if one has started. */
function streamingReply(items: Item[]) {
  const last = items[items.length - 1];
  return last?.kind === 'reply' && last.streaming ? last : undefined;
}

/** A WebSocket event, as the server sends it: its payload's fields and type. */
export type WsEvent = { type: string } & Record<string, unknown>;

const str = (v: unknown) => (typeof v === 'string' ? v : '');

/** The timeline after one WebSocket event. */
export function apply(chat: Chat, ev: WsEvent): Chat {
  const items = [...chat.items];
  switch (ev.type) {
    case 'tool_start':
      items.push({ kind: 'tool', name: str(ev.tool_name), input: ev.tool_input, done: false });
      return { items, busy: true };
    case 'tool_end': {
      const name = str(ev.tool_name);
      const open = lastOpenTool(items, name);
      if (open) items[items.indexOf(open)] = { ...open, output: str(ev.tool_output), done: true };
      else items.push({ kind: 'tool', name, output: str(ev.tool_output), done: true });
      return { items, busy: true };
    }
    case 'sub_agent_start':
      items.push({ kind: 'agent', id: str(ev.sub_agent_id), task: str(ev.task) || undefined, role: str(ev.role) || undefined });
      return { items, busy: true };
    case 'sub_agent_end': {
      const i = items.findLastIndex((x) => x.kind === 'agent' && x.id === ev.sub_agent_id);
      if (i >= 0) items[i] = { ...(items[i] as Extract<Item, { kind: 'agent' }>), status: str(ev.status) || 'done' };
      return { items, busy: true };
    }
    case 'response_chunk': {
      const r = streamingReply(items);
      if (r) items[items.length - 1] = { ...r, text: r.text + str(ev.text) };
      else items.push({ kind: 'reply', text: str(ev.text), streaming: true });
      return { items, busy: true };
    }
    case 'response': {
      const r = streamingReply(items);
      const reply: Item = { kind: 'reply', text: str(ev.text), streaming: false };
      if (r) items[items.length - 1] = reply;
      else items.push(reply);
      return { items, busy: chat.busy };
    }
    case 'done':
      return { items: settle(items), busy: false };
    case 'notice':
      items.push({ kind: 'notice', text: str(ev.text) });
      return { items, busy: chat.busy };
    case 'queued':
      items.push({ kind: 'notice', text: 'Queued: the session is mid-turn, so this message runs after it.' });
      return { items, busy: chat.busy };
    case 'error': {
      items.push(errorItem(str((ev.error as { message?: unknown } | undefined)?.message)));
      return { items: settle(items), busy: false };
    }
    case 'human_input_required':
      if (!items.some((i) => i.kind === 'question' && i.id === ev.request_id)) {
        items.push({
          kind: 'question',
          id: str(ev.request_id),
          question: str(ev.question),
          options: Array.isArray(ev.options) ? ev.options.map(String) : [],
        });
      }
      return { items, busy: chat.busy };
  }
  return chat;
}

/** Ends what a finished turn left open: its streaming reply, its tools. */
function settle(items: Item[]): Item[] {
  return items.map((i) => {
    if (i.kind === 'reply' && i.streaming) return { ...i, streaming: false };
    if (i.kind === 'tool' && !i.done) return { ...i, done: true };
    if (i.kind === 'user' && i.pending) return { ...i, pending: false };
    return i;
  });
}

/** The timeline with the operator's message added as it is sent. */
export function sent(chat: Chat, text: string): Chat {
  return { items: [...chat.items, { kind: 'user', text, pending: true }], busy: true };
}

/** The timeline with a question marked answered. */
export function answered(chat: Chat, id: string, answer: string): Chat {
  return {
    ...chat,
    items: chat.items.map((i) => (i.kind === 'question' && i.id === id ? { ...i, answered: answer } : i)),
  };
}

/**
 * The timeline once a turn is over: the transcript, which has the turn
 * numbers, plus what it does not have. The journal is written off the turn's
 * path, so a transcript read the moment a turn ends can lack some of that
 * turn — its reply is written last. Until the transcript has as many messages
 * and replies as the live view, the live view stays as it is and complete is
 * false, so the caller reads again. Notices and open questions are never in
 * the transcript, and an error is not until its turn is journaled: each is
 * kept at the end of the turn it came in, so it stays in the conversation
 * after the next message is sent. A turn whose error the transcript already
 * has keeps that one instead.
 */
export function reconcile(history: Item[], live: Item[]): { items: Item[]; complete: boolean } {
  const count = (items: Item[], kind: 'user' | 'reply') =>
    items.filter((i) => i.kind === kind && (kind === 'user' || i.text !== '')).length;
  if (count(live, 'user') > count(history, 'user') || count(live, 'reply') > count(history, 'reply')) {
    return { items: live, complete: false };
  }
  // Each kept item, by how many of the operator's messages came before it.
  const kept = new Map<number, Item[]>();
  let users = 0;
  for (const i of live) {
    if (i.kind === 'user') users++;
    else if (i.kind === 'error' || i.kind === 'notice' || (i.kind === 'question' && !i.answered)) {
      kept.set(users, [...(kept.get(users) ?? []), i]);
    }
  }
  // The turns, by the same count, whose error the transcript has.
  const journaled = new Set<number>();
  users = 0;
  for (const i of history) {
    if (i.kind === 'user') users++;
    else if (i.kind === 'error') journaled.add(users);
  }
  const keep = (n: number) => (kept.get(n) ?? []).filter((i) => i.kind !== 'error' || !journaled.has(n));
  const items: Item[] = [];
  users = 0;
  for (const i of history) {
    if (i.kind === 'user') items.push(...keep(users++));
    items.push(i);
  }
  items.push(...keep(users));
  return { items, complete: true };
}

/** Questions waiting for this conversation, as timeline items. */
export function pendingQuestions(qs: Schemas['Question'][], agentId: string): Extract<Item, { kind: 'question' }>[] {
  return qs
    .filter((q) => q.agent_id === agentId)
    .map((q) => ({ kind: 'question' as const, id: q.id, question: q.question, options: q.options ?? [] }));
}
