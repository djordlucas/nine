import type { ComponentChildren } from 'preact';
import { api, errorMessage, type Schemas } from '../api/client';
import { recentEvents } from '../api/events';
import { eventKind, shortId, summarize, timeOf } from '../journal';
import { useLive, useRecent } from '../live';
import styles from './Dashboard.module.css';

/**
 * Home (adr/web-ui.md §5): what needs the operator, what is running, what
 * happened. Each tile loads with the API's GETs and loads again when the
 * event stream reports a change it shows.
 */
export function Dashboard() {
  return (
    <div class={styles.grid}>
      <Attention />
      <Processes />
      <Activity />
      <Conversations />
      <Goals />
      <Notifications />
      <Daemon />
    </div>
  );
}

/** Unwraps an openapi-fetch result, throwing its error message. */
async function get<T>(p: Promise<{ data?: T; error?: unknown }>, what: string): Promise<T> {
  const { data, error } = await p;
  if (data === undefined) throw new Error(errorMessage(error, `Could not load ${what}.`));
  return data;
}

function Tile(props: {
  id: string;
  title: string;
  href?: string;
  count?: number;
  error: string | null;
  loading: boolean;
  children?: ComponentChildren;
}) {
  return (
    <section class={`card ${styles.tile}`} aria-labelledby={`${props.id}-h`}>
      <header class={styles.tileHead}>
        <h2 id={`${props.id}-h`} class={styles.tileTitle}>
          {props.title}
        </h2>
        {props.count !== undefined && props.count > 0 && <span class={styles.count}>{props.count}</span>}
        {props.href && (
          <a class={styles.more} href={props.href} aria-label={`Open ${props.title}`}>
            Open
          </a>
        )}
      </header>
      {props.error && <p class="error">{props.error}</p>}
      {!props.error && props.loading && <p class="muted">Loading…</p>}
      {!props.loading && props.children}
    </section>
  );
}

function Empty({ children }: { children: ComponentChildren }) {
  return <p class="muted">{children}</p>;
}

function ago(iso: string | undefined): string {
  if (!iso) return '';
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return '';
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

/** Pending approvals and unanswered ask_human questions. */
function Attention() {
  const { data, error } = useLive(
    async () => {
      const [caps, qs] = await Promise.all([
        get(api.GET('/capabilities'), 'capability requests'),
        get(api.GET('/questions'), 'questions'),
      ]);
      return {
        requests: (caps.requests ?? []).filter((r) => r.status === 'pending'),
        questions: qs.data,
      };
    },
    ['capability_*', 'human_input_*'],
  );
  const total = data ? data.requests.length + data.questions.length : 0;
  return (
    <Tile id="attention" title="Needs you" href="/inbox" count={total} error={error} loading={!data && !error}>
      {data && total === 0 && <Empty>Nothing is waiting for you.</Empty>}
      {data && total > 0 && (
        <ul class={styles.list}>
          {data.requests.map((r) => (
            <li key={r.id}>
              <span class={`${styles.badge} ${styles.attention}`}>approval</span>
              <span class={styles.text}>
                {r.tool_name ? `${r.tool_name} wants ` : ''}
                <code>{r.capability}</code>
              </span>
            </li>
          ))}
          {data.questions.map((q) => (
            <li key={q.id}>
              <span class={`${styles.badge} ${styles.attention}`}>question</span>
              <span class={styles.text}>{q.question}</span>
            </li>
          ))}
        </ul>
      )}
    </Tile>
  );
}

type Process = Schemas['ProcessInfo'];
type ProcessState = 'running' | 'paused' | 'failing' | 'stopped';

/** A process its budget paused reads as stopped by "budget". */
function processState(p: Process): ProcessState {
  if (p.state === 'stopped' && p.stopped_by === 'budget') return 'paused';
  return p.state;
}

const stateOrder: Record<ProcessState, number> = { failing: 0, paused: 1, running: 2, stopped: 3 };

function Processes() {
  const { data, error } = useLive(
    async () => (await get(api.GET('/processes'), 'processes')).data ?? [],
    ['process_*', 'standing_*'],
  );
  const counts: Record<ProcessState, number> = { running: 0, paused: 0, failing: 0, stopped: 0 };
  for (const p of data ?? []) counts[processState(p)]++;
  const shown = [...(data ?? [])].sort((a, b) => stateOrder[processState(a)] - stateOrder[processState(b)]).slice(0, 5);
  return (
    <Tile id="processes" title="Processes" href="/processes" error={error} loading={!data && !error}>
      {data && data.length === 0 && <Empty>No processes.</Empty>}
      {data && data.length > 0 && (
        <>
          <p class={styles.summary}>
            <span>{counts.running} running</span>
            {counts.failing > 0 && <span class="error">{counts.failing} failing</span>}
            {counts.paused > 0 && <span class={styles.warnText}>{counts.paused} paused</span>}
            {counts.stopped > 0 && <span class="muted">{counts.stopped} stopped</span>}
          </p>
          <ul class={styles.list}>
            {shown.map((p) => (
              <li key={p.id}>
                <span class={`${styles.badge} ${styles[processState(p)]}`}>{processState(p)}</span>
                <span class={styles.text}>{p.id}</span>
                <span class={styles.meta}>{p.trigger}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </Tile>
  );
}

const activityTypes = [
  'turn_start',
  'turn_end',
  'tool_end',
  'sub_agent_start',
  'process_*',
  'standing_*',
  'human_input_*',
  'capability_*',
  'notification',
  'goal_*',
  'workflow_*',
];

/** The latest things that happened, live from the stream. */
function Activity() {
  const events = useRecent(activityTypes, 8, recentEvents);
  return (
    <Tile id="activity" title="Activity" href="/journal" error={null} loading={false}>
      {events.length === 0 && <Empty>Nothing has happened recently.</Empty>}
      {events.length > 0 && (
        <ul class={styles.list}>
          {events.map((e) => (
            <li key={e.seq}>
              <span class={`${styles.badge} ${styles[eventKind(e.type)]}`}>{e.type}</span>
              <span class={styles.text}>{summarize(e) || shortId(e.agent_id)}</span>
              <span class={styles.meta}>{timeOf(e)}</span>
            </li>
          ))}
        </ul>
      )}
    </Tile>
  );
}

function Conversations() {
  const { data, error } = useLive(
    async () => {
      const list = (await get(api.GET('/conversations', { params: { query: { limit: 50 } } }), 'conversations')).data ?? [];
      return [...list].sort((a, b) => (b.updated_at ?? '').localeCompare(a.updated_at ?? '')).slice(0, 5);
    },
    ['turn_end'],
  );
  return (
    <Tile id="conversations" title="Recent conversations" href="/conversations" error={error} loading={!data && !error}>
      {data && data.length === 0 && <Empty>No conversations yet.</Empty>}
      {data && data.length > 0 && (
        <ul class={styles.list}>
          {data.map((c) => (
            <li key={c.id}>
              <span class={styles.text}>{c.name || shortId(c.id ?? '')}</span>
              <span class={styles.meta}>{ago(c.updated_at)}</span>
            </li>
          ))}
        </ul>
      )}
    </Tile>
  );
}

function Goals() {
  const { data, error } = useLive(
    async () => {
      const [goals, workflows] = await Promise.all([
        get(api.GET('/goals', { params: { query: { limit: 1000 } } }), 'goals'),
        get(api.GET('/workflows', { params: { query: { limit: 1000 } } }), 'workflows'),
      ]);
      return {
        goals: (goals.data ?? []).filter((g) => g.status === 'active' && !g.parent_id),
        workflows: (workflows.data ?? []).filter((w) => w.status === 'active'),
      };
    },
    ['goal_*', 'workflow_*'],
  );
  const total = data ? data.goals.length + data.workflows.length : 0;
  return (
    <Tile id="goals" title="Goals & workflows" href="/goals" count={total} error={error} loading={!data && !error}>
      {data && total === 0 && <Empty>No active goals or workflows.</Empty>}
      {data && total > 0 && (
        <ul class={styles.list}>
          {data.goals.slice(0, 4).map((g) => (
            <li key={g.id}>
              <span class={`${styles.badge} ${styles.state}`}>goal</span>
              <span class={styles.text}>{g.description}</span>
            </li>
          ))}
          {data.workflows.slice(0, 4).map((w) => (
            <li key={w.id}>
              <span class={`${styles.badge} ${styles.state}`}>workflow</span>
              <span class={styles.text}>{w.name}</span>
              <span class={styles.meta}>
                {w.completed_steps ?? 0}/{w.steps ?? 0}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Tile>
  );
}

function Notifications() {
  const { data, error } = useLive(
    async () => {
      // all=true: without it the list marks what it returns as seen, and a
      // glance at the dashboard is not reading the inbox.
      const list =
        (await get(api.GET('/notifications', { params: { query: { all: true, limit: 1000 } } }), 'notifications'))
          .data ?? [];
      return [...list].sort((a, b) => (b.created_at ?? '').localeCompare(a.created_at ?? '')).slice(0, 5);
    },
    ['notification'],
  );
  return (
    <Tile id="notifications" title="Notifications" href="/inbox" error={error} loading={!data && !error}>
      {data && data.length === 0 && <Empty>No notifications.</Empty>}
      {data && data.length > 0 && (
        <ul class={styles.list}>
          {data.map((n) => (
            <li key={n.id}>
              <span class={styles.text}>{n.message}</span>
              <span class={styles.meta}>{ago(n.created_at)}</span>
            </li>
          ))}
        </ul>
      )}
    </Tile>
  );
}

function Daemon() {
  const { data, error } = useLive(async () => get(api.GET('/status'), 'the daemon status'), []);
  return (
    <Tile id="status" title="Daemon" href="/system" error={error} loading={!data && !error}>
      {data && (
        <dl class={styles.facts}>
          <dt>Status</dt>
          <dd>{data.status ?? '—'}</dd>
          <dt>Version</dt>
          <dd>{data.version ?? '—'}</dd>
          <dt>Uptime</dt>
          <dd>{data.uptime ?? '—'}</dd>
          <dt>Active sessions</dt>
          <dd>{data.active_sessions ?? 0}</dd>
          <dt>Plugins</dt>
          <dd>{data.plugins?.length ?? 0}</dd>
          <dt>Tools</dt>
          <dd>{data.tools?.length ?? 0}</dd>
        </dl>
      )}
    </Tile>
  );
}
