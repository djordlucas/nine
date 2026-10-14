import type { WsEvent } from '../chat';

/**
 * One conversation's WebSocket (spec/contracts/api.md API-STREAM-3): the turns
 * it runs, whoever started them, and the way to send a turn, answer a
 * question or cancel. A dropped socket reopens after a pause; the view reloads
 * the transcript then, since the socket replays nothing.
 */

export type SocketState = 'connecting' | 'open' | 'closed';

export interface SocketHandlers {
  event(e: WsEvent): void;
  state(s: SocketState): void;
  /** The socket reopened after a drop: what it missed is in the transcript. */
  reopened(): void;
}

/** Swapped in tests. */
export const socketEnv = {
  open: (url: string): WebSocket => new WebSocket(url),
  setTimeout: (f: () => void, ms: number) => setTimeout(f, ms),
  clearTimeout: (t: ReturnType<typeof setTimeout> | undefined) => clearTimeout(t),
};

export function socketURL(id: string): string {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  return `${scheme}://${location.host}/ws/v1/conversations/${encodeURIComponent(id)}/messages`;
}

export class ConversationSocket {
  private ws: WebSocket | null = null;
  private retry: ReturnType<typeof setTimeout> | undefined;
  private closed = false;
  private opened = false;
  private backoff = 0;

  constructor(
    private id: string,
    private on: SocketHandlers,
  ) {
    this.connect();
  }

  private connect() {
    const ws = socketEnv.open(socketURL(this.id));
    this.ws = ws;
    this.on.state('connecting');
    ws.onopen = () => {
      this.backoff = 0;
      this.on.state('open');
      if (this.opened) this.on.reopened();
      this.opened = true;
    };
    ws.onmessage = (m) => {
      try {
        this.on.event(JSON.parse(String(m.data)) as WsEvent);
      } catch {
        // Not JSON: nothing the server sends.
      }
    };
    ws.onclose = () => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.on.state('closed');
      if (this.closed) return;
      this.backoff = Math.min(this.backoff === 0 ? 1000 : this.backoff * 2, 15000);
      this.retry = socketEnv.setTimeout(() => this.connect(), this.backoff);
    };
  }

  /** Sends a client message; false when the socket is not open. */
  send(msg: { type: 'user_turn'; text: string; force_think?: boolean } | { type: 'human_input_answer'; request_id: string; answer: string } | { type: 'cancel' }): boolean {
    if (!this.ws || this.ws.readyState !== 1) return false;
    this.ws.send(JSON.stringify(msg));
    return true;
  }

  close() {
    this.closed = true;
    socketEnv.clearTimeout(this.retry);
    this.ws?.close(1000);
    this.ws = null;
  }
}
