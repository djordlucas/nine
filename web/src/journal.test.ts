import { describe, expect, it } from 'vitest';
import { clip, eventKind, omittedBytes, shortId, summarize } from './journal';
import type { JournalEvent } from './api/events';

function ev(type: string, payload?: unknown): JournalEvent {
  return { seq: 1, agent_id: 'a', turn: 1, type, timestamp: '2026-10-13T12:00:00Z', payload };
}

describe('journal display', () => {
  it('sorts types into kinds', () => {
    expect(eventKind('turn_end')).toBe('turn');
    expect(eventKind('llm_request')).toBe('llm');
    expect(eventKind('tool_http')).toBe('tool');
    expect(eventKind('sub_agent_start')).toBe('agent');
    expect(eventKind('standing_failing')).toBe('process');
    expect(eventKind('capability_requested')).toBe('attention');
    expect(eventKind('capability_decided')).toBe('state');
    expect(eventKind('goal_created')).toBe('state');
    expect(eventKind('something_new')).toBe('other');
  });

  it('summarizes what each event says', () => {
    expect(summarize(ev('turn_start', { input: 'hello\nthere', trigger: 'user' }))).toBe('hello there');
    expect(summarize(ev('turn_start', { input: 'tick', trigger: 'idle' }))).toBe('idle · tick');
    expect(summarize(ev('turn_end', { error: 'model down' }))).toBe('failed: model down');
    expect(summarize(ev('turn_end', { result: 'done', duration_ms: 2500, tool_count: 2 }))).toBe('done · 2.5 s · 2 tools');
    expect(summarize(ev('tool_end', { name: 'fetch', duration_ms: 40 }))).toBe('fetch · 40 ms');
    expect(summarize(ev('capability_requested', { tool: 't', capability: 'net.http' }))).toBe('t · net.http');
    expect(summarize(ev('goal_deleted', { ids: ['a', 'b'] }))).toBe('2 goals');
    expect(summarize(ev('process_paused', { budget: 'turns' }))).toBe('{"budget":"turns"}');
    expect(summarize(ev('turn_start'))).toBe('');
  });

  it('says when the stream left a payload out', () => {
    const e = ev('llm_request', { omitted: true, bytes: 204800 });
    expect(omittedBytes(e)).toBe(204800);
    expect(summarize(e)).toBe('payload omitted (200 KB)');
    expect(omittedBytes(ev('llm_request', { omitted: 'no' }))).toBeNull();
  });

  it('clips and shortens', () => {
    expect(clip('x'.repeat(200), 10)).toBe('xxxxxxxxx…');
    expect(shortId('0123456789abcdef')).toBe('01234567');
    expect(shortId('daemon')).toBe('daemon');
  });
});
