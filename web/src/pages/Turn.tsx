import { useEffect, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { api, errorMessage, type Schemas } from '../api/client';
import { duration, pretty } from '../format';
import { shortId, summarize, timeOf } from '../journal';
import { Markdown } from '../markdown';
import styles from './Turn.module.css';

/**
 * One turn, as the journal recorded it (adr/web-ui.md §5): its input, every
 * model call and tool call with what went in and came out, its sub-agents and
 * its result, then the raw events. Replay here renders the record; it calls
 * no model and no tool.
 */
export function Turn() {
  const { params } = useRoute();
  const id = params.id as string;
  const n = Number(params.n);
  const [replay, setReplay] = useState<Schemas['ReplayTurnResponse'] | null>(null);
  const [events, setEvents] = useState<Schemas['TraceEvent'][] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!Number.isInteger(n) || n < 1) {
      setError('No such turn.');
      return;
    }
    api.POST('/conversations/{id}/replay', { params: { path: { id } }, body: { turn: n } }).then(({ data, error: e }) => {
      if (data) setReplay(data);
      else setError(errorMessage(e, 'Could not load the turn.'));
    });
    api.GET('/conversations/{id}/trace', { params: { path: { id }, query: { turn: n } } }).then(({ data }) => {
      if (data) setEvents(data.events);
    });
  }, [id, n]);

  return (
    <div class={styles.page}>
      <nav class={styles.crumbs} aria-label="Breadcrumb">
        <a href={`/conversations/${id}`}>‹ Conversation {shortId(id)}</a>
        <span class={styles.steps}>
          {n > 1 && <a href={`/conversations/${id}/turns/${n - 1}`}>Turn {n - 1}</a>}
          <a href={`/conversations/${id}/turns/${n + 1}`}>Turn {n + 1}</a>
        </span>
      </nav>
      <h2 class={styles.title}>Turn {n}</h2>
      {error && <p class="error">{error}</p>}
      {!replay && !error && <p class="muted">Loading…</p>}
      {replay && <ReplayView r={replay} />}
      {events && (
        <details class="card">
          <summary>Raw events ({events.length})</summary>
          <ol class={styles.raw}>
            {events.map((e) => (
              <li key={e.seq}>
                <details>
                  <summary>
                    <span class="muted">{timeOf({ ...e, agent_id: id, timestamp: e.timestamp ?? '' })}</span> <code>{e.type}</code>{' '}
                    {summarize({ ...e, agent_id: id, timestamp: e.timestamp ?? '' })}
                  </summary>
                  <pre class={styles.pre}>{pretty(e.payload ?? {})}</pre>
                </details>
              </li>
            ))}
          </ol>
        </details>
      )}
    </div>
  );
}

function ReplayView({ r }: { r: Schemas['ReplayTurnResponse'] }) {
  return (
    <>
      <section class="card" aria-label="Input">
        <h3 class={styles.h}>
          Input {r.trigger && r.trigger !== 'user' && <span class="muted">· {r.trigger}</span>}
        </h3>
        <p class={styles.input}>{r.input ?? ''}</p>
      </section>

      {r.llm_calls.length > 0 && (
        <section aria-label="Model calls">
          <h3 class={styles.h}>Model calls</h3>
          <ol class={styles.cards}>
            {r.llm_calls.map((c) => (
              <li key={c.n} class="card">
                <p class={styles.facts}>
                  <strong>Call {c.n}</strong>
                  {c.stop_reason && <span>{c.stop_reason}</span>}
                  {c.tokens_used !== undefined && (
                    <span>
                      {c.tokens_used}/{c.budget} context tokens
                    </span>
                  )}
                  {c.input_tokens !== undefined && (
                    <span>
                      {c.input_tokens} in · {c.output_tokens} out
                    </span>
                  )}
                  {c.message_count !== undefined && <span>{c.message_count} messages</span>}
                </p>
                {c.text && <Markdown text={c.text} class={styles.md} />}
                {c.tool_calls && c.tool_calls.length > 0 && (
                  <p class="muted">Asked for: {c.tool_calls.map((t) => t.name).join(', ')}</p>
                )}
                {c.system && (
                  <details>
                    <summary>System prompt</summary>
                    <pre class={styles.pre}>{c.system}</pre>
                  </details>
                )}
              </li>
            ))}
          </ol>
        </section>
      )}

      {r.tool_calls.length > 0 && (
        <section aria-label="Tool calls">
          <h3 class={styles.h}>Tool calls</h3>
          <ol class={styles.cards}>
            {r.tool_calls.map((t, i) => (
              <li key={i} class="card">
                <p class={styles.facts}>
                  <strong>
                    <code>{t.name}</code>
                  </strong>
                  {t.duration_ms !== undefined && <span>{duration(t.duration_ms)}</span>}
                  {t.attempts !== undefined && t.attempts > 1 && <span>{t.attempts} attempts</span>}
                  {t.error && <span class="error">failed</span>}
                </p>
                {t.input !== undefined && (
                  <details>
                    <summary>Input</summary>
                    <pre class={styles.pre}>{pretty(t.input)}</pre>
                  </details>
                )}
                {t.output !== undefined && (
                  <details open={!!t.output && t.output.length < 400}>
                    <summary>
                      Output{t.truncated ? ` (truncated, ${t.output_chars ?? '?'} chars)` : ''}
                    </summary>
                    <pre class={styles.pre}>{t.output}</pre>
                  </details>
                )}
                {t.error && <p class="error">{t.error}</p>}
                {t.http && t.http.length > 0 && (
                  <ul class={styles.http}>
                    {t.http.map((h, j) => (
                      <li key={j}>
                        <code>
                          {h.method} {h.url ?? h.host}
                        </code>{' '}
                        <span class={h.error ? 'error' : 'muted'}>{h.error ?? h.status}</span> <span class="muted">{duration(h.duration_ms)}</span>
                      </li>
                    ))}
                  </ul>
                )}
              </li>
            ))}
          </ol>
        </section>
      )}

      {r.sub_agents.length > 0 && (
        <section aria-label="Sub-agents">
          <h3 class={styles.h}>Sub-agents</h3>
          <ul class={styles.cards}>
            {r.sub_agents.map((s) => (
              <li key={s.id} class="card">
                <p class={styles.facts}>
                  <strong>{s.role ?? shortId(s.id)}</strong>
                  {s.status && <span>{s.status}</span>}
                </p>
                {s.task && <p>{s.task}</p>}
                <a href={`/conversations/${s.id}/turns/1`}>Its first turn</a>
              </li>
            ))}
          </ul>
        </section>
      )}

      {r.result && (
        <section class="card" aria-label="Result">
          <h3 class={styles.h}>
            Result
            <span class="muted">
              {' '}
              · {duration(r.result.duration_ms)}
              {r.result.tool_count ? ` · ${r.result.tool_count} tools` : ''}
            </span>
          </h3>
          {r.result.error ? <p class="error">{r.result.error}</p> : <Markdown text={r.result.text ?? ''} class={styles.md} />}
        </section>
      )}
    </>
  );
}
