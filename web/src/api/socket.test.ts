import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ConversationSocket, socketEnv, socketURL } from './socket';

class FakeWS {
  static all: FakeWS[] = [];
  readyState = 0;
  sent: string[] = [];
  closedWith?: number;
  onopen?: () => void;
  onmessage?: (m: { data: string }) => void;
  onclose?: () => void;
  constructor(public url: string) {
    FakeWS.all.push(this);
  }
  send(s: string) {
    this.sent.push(s);
  }
  close(code?: number) {
    this.closedWith = code;
    this.readyState = 3;
    this.onclose?.();
  }
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
  drop() {
    this.readyState = 3;
    this.onclose?.();
  }
}

const timers: (() => void)[] = [];

beforeEach(() => {
  FakeWS.all = [];
  timers.length = 0;
  socketEnv.open = (url) => new FakeWS(url) as unknown as WebSocket;
  socketEnv.setTimeout = (f) => {
    timers.push(f);
    return 0 as unknown as ReturnType<typeof setTimeout>;
  };
  socketEnv.clearTimeout = () => {};
});

function handlers() {
  return { event: vi.fn(), state: vi.fn(), reopened: vi.fn() };
}

describe('conversation socket', () => {
  it('connects to the conversation and relays its events', () => {
    const h = handlers();
    const s = new ConversationSocket('a b', h);
    const ws = FakeWS.all[0];
    expect(ws.url).toBe(socketURL('a b'));
    expect(ws.url).toMatch(/^ws:\/\/.*\/ws\/v1\/conversations\/a%20b\/messages$/);
    expect(s.send({ type: 'cancel' })).toBe(false);
    ws.open();
    expect(h.state).toHaveBeenLastCalledWith('open');
    ws.onmessage?.({ data: '{"type":"response","text":"hi"}' });
    ws.onmessage?.({ data: 'not json' });
    expect(h.event).toHaveBeenCalledTimes(1);
    expect(s.send({ type: 'user_turn', text: 'go' })).toBe(true);
    expect(JSON.parse(ws.sent[0])).toEqual({ type: 'user_turn', text: 'go' });
  });

  it('reopens after a drop and says so', () => {
    const h = handlers();
    new ConversationSocket('a', h);
    FakeWS.all[0].open();
    FakeWS.all[0].drop();
    expect(h.state).toHaveBeenLastCalledWith('closed');
    timers[0]();
    FakeWS.all[1].open();
    expect(h.reopened).toHaveBeenCalledOnce();
  });

  it('stays closed once closed', () => {
    const h = handlers();
    const s = new ConversationSocket('a', h);
    FakeWS.all[0].open();
    s.close();
    expect(FakeWS.all[0].closedWith).toBe(1000);
    expect(timers).toHaveLength(0);
  });
});
