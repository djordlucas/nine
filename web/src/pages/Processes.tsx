import { useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
import { useLive } from '../live';
import { actionsFor, byAttention, processState, stoppedReason, used, type Process } from '../processes';
import { ListHead, Split } from '../components/Split';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import styles from './Processes.module.css';
import { Icon } from '../components/Icon';

const processEvents = ['process_*', 'standing_*', 'goal_status'];

/** A route parameter as written, whether or not the router decoded it. */
export function param(v: string | undefined): string | undefined {
  if (v === undefined) return undefined;
  try {
    return decodeURIComponent(v);
  } catch {
    return v;
  }
}

/** The path of a process; its id may hold a colon or a slash. */
export function processPath(id: string): string {
  return `/processes/${encodeURIComponent(id)}`;
}

/**
 * Processes (adr/web-ui.md §5): the roster beside one process, with its
 * session, budget and health; start, stop and send.
 */
export function Processes() {
  const { params } = useRoute();
  const id = param(params.id as string | undefined);
  return (
    <Split name="processes" noun="processes" open={!!id}>
      <Roster current={id} />
      <section class={split.pane} aria-label="Process">
        {id ? (
          <ProcessDetail key={id} id={id} />
        ) : (
          <p class={`card muted ${split.placeholder}`}>Choose a process.</p>
        )}
      </section>
    </Split>
  );
}

function Roster({ current }: { current?: string }) {
  const { data, error } = useLive(
    async () => {
      const [list, status] = await Promise.all([
        get(api.GET('/processes', { params: { query: { limit: 1000 } } }), 'processes'),
        get(api.GET('/status'), 'the daemon status'),
      ]);
      return { list: byAttention(list.data ?? []), max: status.max_running };
    },
    processEvents,
  );
  const running = data?.list.filter((p) => p.state !== 'stopped').length ?? 0;
  return (
    <nav class={split.list} aria-label="Processes">
      <ListHead>
        <p class={split.heading}>
          {data && (
            <>
              <span>{data.max ? `${running} of ${data.max} running` : `${running} running`}</span>
              <span>{data.list.length} in all</span>
            </>
          )}
        </p>
      </ListHead>
      {error && <p class="error">{error}</p>}
      {!data && !error && <p class="muted">Loading…</p>}
      {data && data.list.length === 0 && (
        <p class="muted">No processes. A goal, a [[process]] block in nine.toml, or the model starts one.</p>
      )}
      {data && data.list.length > 0 && (
        <ul>
          {data.list.map((p) => (
            <li key={p.id}>
              <a href={processPath(p.id)} class={split.row} aria-current={p.id === current ? 'page' : undefined}>
                <span class={`${detail.badge} ${detail[processState(p)]}`}>{processState(p)}</span>
                <span class={split.name}>{p.id}</span>
                <span class={split.meta}>{[p.tool, p.trigger !== '—' ? p.trigger : ''].filter(Boolean).join(' · ')}</span>
              </a>
            </li>
          ))}
        </ul>
      )}
    </nav>
  );
}

function ProcessDetail({ id }: { id: string }) {
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  const { data: p, error } = useLive(
    async () => {
      const { data, error: e, response } = await api.GET('/processes/{id}', { params: { path: { id } } });
      if (response.status === 404) return null;
      if (!data) throw new Error(errorMessage(e, 'Could not load the process.'));
      return data;
    },
    processEvents,
  );

  const act = async (run: () => Promise<{ data?: { message?: string }; error?: unknown }>, done: string) => {
    setBusy(true);
    setOutcome(null);
    const { data, error: e } = await run();
    setBusy(false);
    setOutcome(data ? { ok: true, text: data.message || done } : { ok: false, text: errorMessage(e, 'The daemon refused.') });
  };

  if (error) return <p class="card error">{error}</p>;
  if (p === null) return <p class="card muted">No process {id}. It may have been removed.</p>;
  if (!p) return <p class="card muted">Loading…</p>;

  const can = actionsFor(p);
  const path = { params: { path: { id } } };
  const start = () => act(() => api.POST('/processes/{id}/start', path), 'Started.');
  const stop = () => {
    if (!confirm(`Stop ${id}? It stays stopped until you start it again.`)) return;
    void act(() => api.POST('/processes/{id}/stop', path), 'Stopped.');
  };
  const resume = (goal: string) =>
    act(
      async () => {
        const r = await api.POST('/goals/{id}/status', { params: { path: { id: goal } }, body: { status: 'active' } });
        return { data: r.data ? { message: 'Goal resumed; its processes start again.' } : undefined, error: r.error };
      },
      'Goal resumed.',
    );

  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href="/processes" aria-label="All processes">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{p.id}</h2>
          <span class="muted">
            <span class={`${detail.badge} ${detail[processState(p)]}`}>{processState(p)}</span> {p.mode} · {p.tool}
          </span>
        </div>
        <div class={detail.actions}>
          {can.resumeGoal && (
            <button type="button" class={`${detail.action} ${detail.primary}`} disabled={busy} onClick={() => void resume(can.resumeGoal!)}>
              Resume goal
            </button>
          )}
          {can.start && (
            <button
              type="button"
              class={`${detail.action} ${detail.iconAction}`}
              disabled={busy}
              onClick={() => void start()}
              aria-label={p.state === 'failing' ? 'Retry now' : 'Start'}
              title={p.state === 'failing' ? 'Retry now' : 'Start this process'}
            >
              <Icon name={p.state === 'failing' ? 'reload' : 'play'} size={18} />
            </button>
          )}
          {can.stop && (
            <button type="button" class={`${detail.action} ${detail.iconAction}`} disabled={busy} onClick={stop} aria-label="Stop" title="Stop this process">
              <Icon name="stop" size={18} />
            </button>
          )}
        </div>
      </header>

      {outcome && (
        <p class={outcome.ok ? detail.notice : 'error'} role="status">
          {outcome.text}
        </p>
      )}
      {p.state === 'stopped' && <p class={detail.notice}>{stoppedReason(p)}</p>}
      {p.state === 'failing' && p.last_error && (
        <p class={`${detail.notice} error`}>
          {p.failures ?? 0} failure(s) in a row: {p.last_error}
        </p>
      )}

      <Facts p={p} />
      <Budget p={p} />
      {can.send && <Send id={id} />}
      <Recent p={p} />
    </article>
  );
}

function Facts({ p }: { p: Process }) {
  return (
    <dl class={detail.facts}>
      <dt>Trigger</dt>
      <dd>{p.trigger}</dd>
      {p.role && (
        <>
          <dt>Role</dt>
          <dd>{p.role}</dd>
        </>
      )}
      {p.session && (
        <>
          <dt>Session</dt>
          <dd>
            <code>{p.session}</code>
            {p.attached ? ' (shared)' : ''}
          </dd>
        </>
      )}
      {p.goal && (
        <>
          <dt>Goal</dt>
          <dd>
            <a href={`/goals/${encodeURIComponent(p.goal)}`}>{p.goal}</a>
          </dd>
        </>
      )}
      {p.report_to && (
        <>
          <dt>Reports to</dt>
          <dd>
            <a href={processPath(p.report_to)}>{p.report_to}</a>
          </dd>
        </>
      )}
      <dt>Calls</dt>
      <dd>
        {p.calls ?? 0}
        {p.cycles ? `, ${p.cycles} cycles` : ''}
      </dd>
      {p.last_call_at && (
        <>
          <dt>Last call</dt>
          <dd>{ago(p.last_call_at)}</dd>
        </>
      )}
      {p.next_at && p.state !== 'stopped' && (
        <>
          <dt>Next</dt>
          <dd>{new Date(p.next_at).toLocaleString()}</dd>
        </>
      )}
      {p.stopped_at && p.state === 'stopped' && (
        <>
          <dt>Stopped</dt>
          <dd>{ago(p.stopped_at)}</dd>
        </>
      )}
      <dt>Written by</dt>
      <dd>{p.generated ? 'Nine' : 'the operator'}</dd>
    </dl>
  );
}

function Budget({ p }: { p: Process }) {
  const b = p.budget;
  return (
    <section class={detail.section} aria-labelledby="budget-h">
      <h3 id="budget-h">Budget today</h3>
      <div class={styles.meters}>
        <Meter label="Turns" n={b.turns} of={b.turns_per_day} />
        <Meter label="Tokens" n={b.tokens} of={b.tokens_per_day} />
      </div>
      {b.resets_at && <p class="muted">The day ends {new Date(b.resets_at).toLocaleString()}.</p>}
    </section>
  );
}

function Meter({ label, n, of }: { label: string; n: number; of: number }) {
  const f = used(n, of);
  return (
    <label class={styles.meter}>
      <span>
        {label} <span class="muted">{of > 0 ? `${n.toLocaleString()} of ${of.toLocaleString()}` : `${n.toLocaleString()}, no limit`}</span>
      </span>
      <meter min={0} max={1} high={0.8} value={f} />
    </label>
  );
}

function Send({ id }: { id: string }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  const send = async (e: Event) => {
    e.preventDefault();
    if (!text.trim()) return;
    setBusy(true);
    setOutcome(null);
    const { data, error } = await api.POST('/processes/{id}/messages', { params: { path: { id } }, body: { text } });
    setBusy(false);
    if (data) {
      setText('');
      setOutcome({ ok: true, text: data.message || 'Sent.' });
    } else {
      setOutcome({ ok: false, text: errorMessage(error, 'The process could not take the message.') });
    }
  };
  return (
    <section class={detail.section} aria-labelledby="send-h">
      <h3 id="send-h">Message it</h3>
      <form class={styles.send} onSubmit={(e) => void send(e)}>
        <textarea
          class="input"
          rows={2}
          aria-label="Message to the process"
          placeholder="It receives this as its next trigger."
          value={text}
          onInput={(e) => setText(e.currentTarget.value)}
        />
        <button type="submit" class="button" disabled={busy || !text.trim()}>
          Send
        </button>
      </form>
      {outcome && (
        <p class={outcome.ok ? 'muted' : 'error'} role="status">
          {outcome.text}
        </p>
      )}
    </section>
  );
}

function Recent({ p }: { p: Process }) {
  const recent = [...(p.recent ?? [])].reverse();
  return (
    <section class={detail.section} aria-labelledby="recent-h">
      <h3 id="recent-h">Recent activity</h3>
      {recent.length === 0 && <p class="muted">Nothing yet.</p>}
      {recent.length > 0 && (
        <ol class={styles.recent}>
          {recent.map((r, i) => (
            <li key={i}>
              <span class={styles.outcome}>{r.outcome}</span>
              <span class={styles.what}>{r.detail}</span>
              <time class="muted" dateTime={r.at}>
                {ago(r.at)}
              </time>
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}
