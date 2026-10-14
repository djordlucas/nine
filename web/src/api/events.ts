import { signal } from '@preact/signals';
import { checkSession, type Schemas } from './client';

/**
 * The daemon-wide event stream (adr/web-ui.md §6): one EventSource per tab on
 * GET /api/v1/events, shared by every view that wants live data. Views
 * subscribe; the stream keeps the most recent events for the Journal.
 */

export type JournalEvent = Schemas['JournalEvent'];

/** live: connected; connecting: (re)opening; offline: waiting to retry. */
export type StreamState = 'connecting' | 'live' | 'offline';

export const streamState = signal<StreamState>('offline');

export interface Listener {
  /** A journal event, in seq order, each once. */
  event?(e: JournalEvent): void;
  /**
   * The stream could not resume where it left off (the events were scrubbed,
   * or it was away too long): what a view built from events may be
   * incomplete, so it reloads.
   */
  reset?(): void;
}

/** How many events the stream keeps for the Journal. */
export const BUFFER = 2000;
/** How many earlier events a fresh stream asks for. */
export const TAIL = 200;

// EventSource.CONNECTING, which a test environment without EventSource lacks.
const CONNECTING = 0;

const listeners = new Set<Listener>();
let buffer: JournalEvent[] = [];
let source: EventSource | null = null;
let lastSeq = 0;
let retry: ReturnType<typeof setTimeout> | undefined;
let backoff = 0;

/** Swapped in tests. */
export const env = {
  open: (url: string): EventSource => new EventSource(url),
  setTimeout: (f: () => void, ms: number) => setTimeout(f, ms),
  clearTimeout: (t: ReturnType<typeof setTimeout> | undefined) => clearTimeout(t),
};

/** Registers a listener; returns the function that removes it. */
export function subscribe(l: Listener): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

/** The events kept so far, oldest first. */
export function recentEvents(): readonly JournalEvent[] {
  return buffer;
}

/** Opens the stream, if it is not open. */
export function connect(): void {
  if (source) return;
  env.clearTimeout(retry);
  retry = undefined;
  // A new EventSource has no Last-Event-ID of its own; after names where the
  // last one stopped. A browser reconnecting the same EventSource sends the
  // header itself, which the server prefers.
  const query = lastSeq > 0 ? `after=${lastSeq}` : `tail=${TAIL}`;
  const es = env.open(`/api/v1/events?${query}`);
  source = es;
  streamState.value = 'connecting';

  es.addEventListener('connected', (m) => {
    const { seq } = JSON.parse((m as MessageEvent).data) as Schemas['EventsConnected'];
    if (lastSeq === 0) lastSeq = seq;
    backoff = 0;
    streamState.value = 'live';
  });
  es.addEventListener('journal', (m) => {
    const e = JSON.parse((m as MessageEvent).data) as JournalEvent;
    if (e.seq <= lastSeq) return;
    lastSeq = e.seq;
    buffer.push(e);
    if (buffer.length > BUFFER) buffer = buffer.slice(buffer.length - BUFFER);
    for (const l of listeners) l.event?.(e);
  });
  es.addEventListener('reset', (m) => {
    const { seq } = JSON.parse((m as MessageEvent).data) as Schemas['EventsReset'];
    lastSeq = seq;
    buffer = [];
    for (const l of listeners) l.reset?.();
  });
  es.addEventListener('error', (m) => {
    // Two things arrive as "error": the server's own error event (the daemon
    // ended the stream), which carries data, and the EventSource's, when the
    // connection drops. A dropped connection the browser retries itself is
    // still CONNECTING; one it gave up on (a 401, a 503) is CLOSED, and is
    // reopened here.
    const fromServer = m instanceof MessageEvent && typeof m.data === 'string';
    if (!fromServer && es.readyState === CONNECTING) {
      streamState.value = 'connecting';
      return;
    }
    es.close();
    if (source === es) source = null;
    streamState.value = 'offline';
    // A 401 shows here only as a closed stream; asking the session endpoint
    // turns it into the sign-in page.
    void checkSession();
    backoff = Math.min(backoff === 0 ? 2000 : backoff * 2, 30000);
    retry = env.setTimeout(() => {
      retry = undefined;
      connect();
    }, backoff);
  });
}

/** Closes the stream and forgets what it held: on sign-out. */
export function disconnect(): void {
  env.clearTimeout(retry);
  retry = undefined;
  source?.close();
  source = null;
  lastSeq = 0;
  backoff = 0;
  buffer = [];
  streamState.value = 'offline';
}

/** Whether type matches a pattern: exact, or a prefix ending in *. */
export function typeMatches(type: string, patterns: readonly string[]): boolean {
  return patterns.some((p) => (p.endsWith('*') ? type.startsWith(p.slice(0, -1)) : type === p));
}
