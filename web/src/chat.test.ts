import { describe, expect, it } from 'vitest';
import { answered, apply, empty, fromHistory, pendingQuestions, reconcile, sent, type Chat } from './chat';

const run = (events: { type: string; [k: string]: unknown }[], start: Chat = empty) => events.reduce(apply, start);

describe('chat timeline', () => {
  it('builds from the transcript, pairing tool calls and keeping turn numbers', () => {
    const items = fromHistory([
      { type: 'user_turn', agent_id: 'a', text: 'hi', turn_number: 1 },
      { type: 'tool_start', agent_id: 'a', tool_name: 'skill_list', tool_input: {} },
      { type: 'tool_end', agent_id: 'a', tool_name: 'skill_list', tool_output: '[]' },
      { type: 'sub_agent_start', agent_id: 'a', sub_agent_id: 's1', text: 'look', role: 'researcher' },
      { type: 'sub_agent_end', agent_id: 'a', sub_agent_id: 's1', status: 'done' },
      { type: 'response', agent_id: 'a', text: 'hello', turn_number: 1 },
      { type: 'user_turn', agent_id: 'a', text: 'again', turn_number: 2 },
      { type: 'tool_start', agent_id: 'a', tool_name: 'cut_off' },
    ]);
    expect(items.map((i) => i.kind)).toEqual(['user', 'tool', 'agent', 'reply', 'user', 'tool']);
    expect(items[1]).toMatchObject({ name: 'skill_list', output: '[]', done: true });
    expect(items[2]).toMatchObject({ role: 'researcher', status: 'done' });
    expect(items[3]).toMatchObject({ text: 'hello', turn: 1, streaming: false });
    // A tool the transcript never saw end is not shown as running.
    expect(items[5]).toMatchObject({ done: true });
  });

  it('streams a reply, then settles it on done', () => {
    let c = sent(empty, 'hi');
    expect(c.busy).toBe(true);
    c = run([{ type: 'response_chunk', text: 'Hel' }, { type: 'response_chunk', text: 'lo' }], c);
    expect(c.items[1]).toEqual({ kind: 'reply', text: 'Hello', streaming: true });
    c = run([{ type: 'response', text: 'Hello.' }, { type: 'done' }], c);
    expect(c.items[1]).toEqual({ kind: 'reply', text: 'Hello.', streaming: false });
    expect(c.items[0]).toMatchObject({ pending: false });
    expect(c.busy).toBe(false);
  });

  it('shows tools as they run and a turn another client started', () => {
    const c = run([
      { type: 'tool_start', tool_name: 'fetch', tool_input: { url: 'x' } },
      { type: 'tool_end', tool_name: 'fetch', tool_output: 'ok' },
      { type: 'response', text: 'done' },
    ]);
    expect(c.busy).toBe(true);
    expect(c.items[0]).toEqual({ kind: 'tool', name: 'fetch', input: { url: 'x' }, output: 'ok', done: true });
  });

  it('ends the turn on an error, and says plainly when it was cancelled', () => {
    let c = run([{ type: 'response_chunk', text: 'tick' }]);
    c = apply(c, { type: 'error', error: { code: 'server_error', message: 'turn cancelled' } });
    expect(c.busy).toBe(false);
    expect(c.items[1]).toEqual({ kind: 'error', text: 'Cancelled.', cancelled: true });
    expect(c.items[0]).toMatchObject({ streaming: false });
    c = apply(c, { type: 'error', error: { code: 'server_error', message: 'model unreachable' } });
    expect(c.items[2]).toEqual({ kind: 'error', text: 'model unreachable', cancelled: false });
  });

  it('asks and records an answer', () => {
    let c = run([{ type: 'human_input_required', request_id: 'q1', question: 'Which?', options: ['A', 'B'] }]);
    c = apply(c, { type: 'human_input_required', request_id: 'q1', question: 'Which?' });
    expect(c.items.filter((i) => i.kind === 'question')).toHaveLength(1);
    c = answered(c, 'q1', 'A');
    expect(c.items[0]).toMatchObject({ answered: 'A' });
  });

  it('notes a queued message and ignores what it does not know', () => {
    const c = run([{ type: 'queued', text: 'queued' }, { type: 'something_new' }]);
    expect(c.items).toHaveLength(1);
    expect(c.items[0].kind).toBe('notice');
  });

  it('keeps errors and open questions when the transcript replaces the live view', () => {
    const live = run([{ type: 'error', error: { message: 'turn cancelled' } }], sent(empty, 'go'));
    const withQ = apply(live, { type: 'human_input_required', request_id: 'q', question: '?' });
    const history = fromHistory([{ type: 'user_turn', agent_id: 'a', text: 'go', turn_number: 3 }]);
    const merged = reconcile(history, withQ.items);
    expect(merged.complete).toBe(true);
    expect(merged.items.map((i) => i.kind)).toEqual(['user', 'error', 'question']);
    expect(merged.items[0]).toMatchObject({ turn: 3 });
  });

  it('keeps an error in its own turn after the next message', () => {
    let live = run([{ type: 'error', error: { message: 'llm: mistral: HTTP 404' } }], sent(empty, 'one'));
    live = run([{ type: 'response', text: 'ok' }, { type: 'done' }], sent(live, 'two'));
    const history = fromHistory([
      { type: 'user_turn', agent_id: 'a', text: 'one', turn_number: 1 },
      { type: 'user_turn', agent_id: 'a', text: 'two', turn_number: 2 },
      { type: 'response', agent_id: 'a', text: 'ok', turn_number: 2 },
    ]);
    const merged = reconcile(history, live.items);
    expect(merged.items.map((i) => i.kind)).toEqual(['user', 'error', 'user', 'reply']);
  });

  it('shows a journaled error after a reload, once, in its turn', () => {
    const history = fromHistory([
      { type: 'user_turn', agent_id: 'a', text: 'one', turn_number: 1 },
      { type: 'turn_error', agent_id: 'a', text: 'llm: mistral: HTTP 404', turn_number: 1 },
      { type: 'user_turn', agent_id: 'a', text: 'two', turn_number: 2 },
      { type: 'turn_error', agent_id: 'a', text: 'turn cancelled', turn_number: 2 },
    ]);
    expect(history.slice(1, 2)).toEqual([{ kind: 'error', text: 'llm: mistral: HTTP 404', cancelled: false }]);
    expect(history[3]).toEqual({ kind: 'error', text: 'Cancelled.', cancelled: true });
    // The page that saw the error live does not show it twice.
    const live = run([{ type: 'error', error: { message: 'llm: mistral: HTTP 404' } }], sent(empty, 'one'));
    const merged = reconcile(history.slice(0, 2), live.items);
    expect(merged.items.map((i) => i.kind)).toEqual(['user', 'error']);
  });

  it('keeps live messages the transcript has not caught up with', () => {
    let live = run([{ type: 'response', text: 'first' }, { type: 'done' }], sent(empty, 'one'));
    live = run([{ type: 'response', text: 'second' }, { type: 'done' }], sent(live, 'two'));
    const history = fromHistory([
      { type: 'user_turn', agent_id: 'a', text: 'one', turn_number: 1 },
      { type: 'response', agent_id: 'a', text: 'first', turn_number: 1 },
    ]);
    const merged = reconcile(history, live.items);
    expect(merged.complete).toBe(false);
    expect(merged.items).toBe(live.items);
    // The turn's message is journaled before its reply.
    const half = reconcile([...history, ...fromHistory([{ type: 'user_turn', agent_id: 'a', text: 'two', turn_number: 2 }])], live.items);
    expect(half.complete).toBe(false);
  });

  it('takes a conversation’s own pending questions', () => {
    const qs = pendingQuestions(
      [
        { id: 'q1', agent_id: 'a', question: 'One?', created_at: '', expires_at: '' },
        { id: 'q2', agent_id: 'b', question: 'Two?', options: ['x'], created_at: '', expires_at: '' },
      ],
      'b',
    );
    expect(qs).toEqual([{ kind: 'question', id: 'q2', question: 'Two?', options: ['x'] }]);
  });
});
