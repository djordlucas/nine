import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// The session check a closed stream triggers goes through fetch.
vi.stubGlobal('fetch', async () => new Response(JSON.stringify({ auth_required: true, authenticated: true }), { status: 200 }));

const events = await import('./events');

/** Stands in for the browser's EventSource. */
class FakeSource {
  static all: FakeSource[] = [];
  readyState = 0;
  closed = false;
  private handlers = new Map<string, ((e: Event) => void)[]>();
  constructor(public url: string) {
    FakeSource.all.push(this);
  }
  addEventListener(name: string, f: (e: Event) => void) {
    this.handlers.set(name, [...(this.handlers.get(name) ?? []), f]);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  emit(name: string, data?: unknown) {
    const e = data === undefined ? new Event(name) : new MessageEvent(name, { data: JSON.stringify(data) });
    for (const f of this.handlers.get(name) ?? []) f(e);
  }
}

function journal(seq: number, type = 'turn_start', agent_id = 'a') {
  return { seq, agent_id, turn: 1, type, timestamp: '2026-10-13T12:00:00Z' };
}

const timers: (() => void)[] = [];

beforeEach(() => {
  FakeSource.all = [];
  timers.length = 0;
  events.env.open = (url) => new FakeSource(url) as unknown as EventSource;
  events.env.setTimeout = (f) => {
    timers.push(f);
    return timers.length as unknown as ReturnType<typeof setTimeout>;
  };
  events.env.clearTimeout = () => {};
});

afterEach(() => events.disconnect());

describe('event stream', () => {
  it('opens with a tail, goes live, and hands events to listeners in order', () => {
    const got: number[] = [];
    const off = events.subscribe({ event: (e) => got.push(e.seq) });
    events.connect();
    const es = FakeSource.all[0];
    expect(es.url).toBe(`/api/v1/events?tail=${events.TAIL}`);
    expect(events.streamState.value).toBe('connecting');

    es.emit('connected', { seq: 10 });
    expect(events.streamState.value).toBe('live');
    es.emit('journal', journal(11));
    es.emit('journal', journal(12));
    es.emit('journal', journal(12)); // a replay of what it has: dropped
    expect(got).toEqual([11, 12]);
    expect(events.recentEvents().map((e) => e.seq)).toEqual([11, 12]);
    off();
  });

  it('opens one stream however often it is asked', () => {
    events.connect();
    events.connect();
    expect(FakeSource.all).toHaveLength(1);
  });

  it('keeps a bounded buffer', () => {
    events.connect();
    const es = FakeSource.all[0];
    es.emit('connected', { seq: 0 });
    for (let i = 1; i <= events.BUFFER + 5; i++) es.emit('journal', journal(i));
    const kept = events.recentEvents();
    expect(kept).toHaveLength(events.BUFFER);
    expect(kept[0].seq).toBe(6);
  });

  it('tells listeners to reload on a reset and forgets what it held', () => {
    const reset = vi.fn();
    const off = events.subscribe({ reset });
    events.connect();
    const es = FakeSource.all[0];
    es.emit('connected', { seq: 1 });
    es.emit('journal', journal(2));
    es.emit('reset', { seq: 900 });
    expect(reset).toHaveBeenCalledOnce();
    expect(events.recentEvents()).toEqual([]);
    es.emit('journal', journal(901));
    expect(events.recentEvents().map((e) => e.seq)).toEqual([901]);
    off();
  });

  it('leaves a dropped connection to the browser to retry', () => {
    events.connect();
    const es = FakeSource.all[0];
    es.emit('connected', { seq: 1 });
    es.readyState = 0;
    es.emit('error');
    expect(events.streamState.value).toBe('connecting');
    expect(es.closed).toBe(false);
    expect(timers).toHaveLength(0);
  });

  it('reopens a stream the browser gave up on, after the last event it had', () => {
    events.connect();
    const es = FakeSource.all[0];
    es.emit('connected', { seq: 1 });
    es.emit('journal', journal(7));
    es.readyState = 2;
    es.emit('error');
    expect(events.streamState.value).toBe('offline');
    expect(timers).toHaveLength(1);
    timers[0]();
    expect(FakeSource.all).toHaveLength(2);
    expect(FakeSource.all[1].url).toBe('/api/v1/events?after=7');
  });

  it('treats the server ending the stream as a drop to recover from', () => {
    events.connect();
    const es = FakeSource.all[0];
    es.emit('connected', { seq: 3 });
    es.emit('error', { error: { code: 'service_unavailable', message: 'daemon closed the stream' } });
    expect(es.closed).toBe(true);
    expect(timers).toHaveLength(1);
    timers[0]();
    expect(FakeSource.all[1].url).toBe('/api/v1/events?after=3');
  });

  it('matches types exactly or by a * prefix', () => {
    expect(events.typeMatches('goal_created', ['goal_*'])).toBe(true);
    expect(events.typeMatches('goal_created', ['goal_status'])).toBe(false);
    expect(events.typeMatches('notification', ['notification'])).toBe(true);
    expect(events.typeMatches('turn_end', [])).toBe(false);
  });
});
