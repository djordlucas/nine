import { useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get, type Schemas } from '../api/client';
import { useLive } from '../live';
import { Markdown } from '../markdown';
import { docLink, topicPath, type DocKind } from '../system';
import { ask } from '../components/Confirm';
import { ListHead, Split } from '../components/Split';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import { param } from './Processes';
import tools from './Tools.module.css';
import styles from './System.module.css';
import { Toml } from '../components/Json';
import { Icon } from '../components/Icon';

type Session = Schemas['SessionInfo'];

const views = [
  { path: '/system', label: 'Overview' },
  { path: '/system/config', label: 'Config' },
  { path: '/system/sessions', label: 'Sessions' },
  { path: '/system/reflections', label: 'Reflections' },
  { path: '/system/docs', label: 'Docs' },
  { path: '/system/spec', label: 'Spec' },
];

/**
 * System (adr/web-ui.md §5): the daemon's status and a backup to download;
 * every session, reflections, and the documentation and specification the
 * build embeds; and the operations only the CLI performs.
 */
export function System() {
  const { path } = useLocation();
  const { params } = useRoute();
  const id = param(params.id as string | undefined);
  const view = views.slice(1).find((v) => path === v.path || path.startsWith(v.path + '/'))?.path ?? '/system';
  return (
    <div class={tools.section}>
      <nav class={tools.views} aria-label="System views">
        {views.map((v) => (
          <a key={v.path} href={v.path} class={tools.view} aria-current={v.path === view ? 'page' : undefined}>
            {v.label}
          </a>
        ))}
      </nav>
      {view === '/system' && <Overview />}
      {view === '/system/config' && <ConfigView />}
      {view === '/system/sessions' && <SessionsView id={id} />}
      {view === '/system/reflections' && <Reflections />}
      {view === '/system/docs' && <DocsView kind="docs" topic={id} />}
      {view === '/system/spec' && <DocsView kind="spec" topic={id} />}
    </div>
  );
}

/**
 * The configuration the daemon runs with, as it reports it: the settings that
 * are set, credentials hidden. Read-only; editing is for a later version.
 */
function ConfigView() {
  const { data, error } = useLive(async () => (await get(api.GET('/config'), 'the configuration')).toml, []);
  return (
    <section class={`card ${styles.config}`} aria-labelledby="config-h">
      <h2 id="config-h" class={styles.title}>
        Configuration
      </h2>
      <p class="muted">
        What the daemon runs with: its config file, then the <code>NINE_*</code> environment variables. Only what is
        set is shown; everything else takes its default. Keys and tokens are hidden. Changing a setting means editing
        the file and restarting Nine.
      </p>
      {error && <p class="error">{error}</p>}
      {!data && !error && <p class="muted">Loading…</p>}
      {data && <Toml text={data} class={styles.toml} />}
    </section>
  );
}

function Overview() {
  const { data, error } = useLive(async () => get(api.GET('/status'), 'the daemon status'), ['process_*', 'standing_*']);
  return (
    <div class={styles.overview}>
      <section class={`card ${detail.detail}`} aria-labelledby="daemon-h">
        <h2 id="daemon-h" class={styles.title}>
          Daemon
        </h2>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && (
          <dl class={detail.facts}>
            <dt>Status</dt>
            <dd>{data.status ?? '—'}</dd>
            <dt>Version</dt>
            <dd>{data.version ?? '—'}</dd>
            <dt>Uptime</dt>
            <dd>{data.uptime ?? '—'}</dd>
            <dt>Active sessions</dt>
            <dd>{data.active_sessions ?? 0}</dd>
            {data.max_running !== undefined && (
              <>
                <dt>Processes at once</dt>
                <dd>up to {data.max_running}</dd>
              </>
            )}
            <dt>Plugins</dt>
            <dd>
              <a href="/tools/plugins">{data.plugins?.length ?? 0}</a>
            </dd>
          </dl>
        )}
        <p class="muted">
          Plugins and skills are under <a href="/tools/plugins">Tools</a>.
        </p>
      </section>

      <section class={`card ${detail.detail}`} aria-labelledby="backup-h">
        <h2 id="backup-h" class={styles.title}>
          Backup
        </h2>
        <p>
          A consistent snapshot, written while Nine runs. The workspace archive also holds the files the store refers
          to, so the two stay in step.
        </p>
        <div class={detail.actions}>
          <a class={`${detail.action} ${detail.primary} ${styles.linkButton}`} href="/api/v1/backup" download>
            Download the store (.db)
          </a>
          <a class={`${detail.action} ${styles.linkButton}`} href="/api/v1/backup?with=workspace" download>
            With the workspace (.tar.gz)
          </a>
        </div>
      </section>

      <section class={`card ${detail.detail}`} aria-labelledby="cli-h">
        <h2 id="cli-h" class={styles.title}>
          In the CLI only
        </h2>
        <p class="muted">These need the machine Nine runs on (adr/web-ui.md §4).</p>
        <dl class={detail.facts}>
          <dt>
            <code>nine restore &lt;snapshot&gt;</code>
          </dt>
          <dd>Puts a backup back. The daemon must be stopped first, since it replaces the store this page reads.</dd>
          <dt>
            <code>nine daemon</code>, <code>nine api serve</code>
          </dt>
          <dd>Start the processes that serve this page.</dd>
          <dt>
            <code>nine tool|plugin|skills validate &lt;path&gt;</code>
          </dt>
          <dd>Check a tool, plugin or skill on disk before it is installed.</dd>
        </dl>
      </section>
    </div>
  );
}

/** Seconds as a short age. */
function age(seconds: number): string {
  if (seconds < 60) return 'just now';
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} h ago`;
  return `${Math.floor(seconds / 86400)} d ago`;
}

function sessionPath(id: string): string {
  return `/system/sessions/${encodeURIComponent(id)}`;
}

function SessionsView({ id }: { id?: string }) {
  const [deleted, setDeleted] = useState(0);
  const { data, error } = useLive(
    async () => {
      const list = (await get(api.GET('/sessions', { params: { query: { limit: 1000 } } }), 'sessions')).data ?? [];
      return [...list].sort((a, b) => a.age_seconds - b.age_seconds);
    },
    ['turn_end', 'process_*', 'standing_*'],
    deleted,
  );
  const [filter, setFilter] = useState('');
  const f = filter.trim().toLowerCase();
  const shown = (data ?? []).filter((s) => !f || `${s.id} ${s.name ?? ''} ${s.status}`.toLowerCase().includes(f));
  return (
    <Split name="sessions" noun="sessions" open={!!id}>
      <nav class={split.list} aria-label="Sessions">
        <ListHead>
          <input
            class="input"
            type="search"
            aria-label="Filter sessions"
            placeholder="Filter sessions"
            value={filter}
            onInput={(e) => setFilter(e.currentTarget.value)}
          />
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && shown.length === 0 && <p class="empty">No session matches.</p>}
        {shown.length > 0 && (
          <ul>
            {shown.map((s) => (
              <li key={s.id}>
                <a href={sessionPath(s.id)} class={split.row} aria-current={s.id === id ? 'page' : undefined}>
                  <span class={detail.badge}>{s.status}</span>
                  <span class={split.name}>{s.name || s.id}</span>
                  <span class={split.meta}>
                    {[age(s.age_seconds), `${s.events ?? 0} events`, s.attached ? 'attached' : '', s.protected ? 'kept' : '']
                      .filter(Boolean)
                      .join(' · ')}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Session">
        {id ? (
          <SessionDetail key={id} id={id} session={data?.find((s) => s.id === id)} loaded={!!data} onDeleted={() => setDeleted((n) => n + 1)} />
        ) : (
          <p class="empty">Every session Nine holds, process sessions included. Choose one.</p>
        )}
      </section>
    </Split>
  );
}

function SessionDetail(props: { id: string; session?: Session; loaded: boolean; onDeleted: () => void }) {
  const { route } = useLocation();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const s = props.session;
  if (!s) return <p class="card muted">{props.loaded ? `No session ${props.id}.` : 'Loading…'}</p>;
  const remove = async () => {
    const ok = await ask({
      title: `Delete ${s.name || s.id}?`,
      message: `Its transcript, its ${s.events ?? 0} journal events, its notifications, tool state and jobs go. This cannot be undone. Ending a session instead keeps all of it.`,
      confirm: 'Delete',
      danger: true,
    });
    if (!ok) return;
    setBusy(true);
    setError(null);
    const { data, error: e } = await api.DELETE('/sessions/{id}', { params: { path: { id: s.id } } });
    setBusy(false);
    if (!data) {
      setError(errorMessage(e, 'Could not delete the session.'));
      return;
    }
    props.onDeleted();
    route('/system/sessions');
  };
  const isProcess = s.id.includes(':');
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href="/system/sessions" aria-label="All sessions">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{s.name || s.id}</h2>
          <span class="muted">{s.status}</span>
        </div>
        <div class={detail.actions}>
          <button type="button" class={`${detail.action} ${detail.iconAction} ${detail.danger}`} disabled={busy} onClick={() => void remove()} aria-label="Delete" title="Delete this session">
            <Icon name="trash" size={16} />
          </button>
        </div>
      </header>
      {error && <p class="error">{error}</p>}
      <dl class={detail.facts}>
        <dt>Id</dt>
        <dd>
          <code>{s.id}</code>
        </dd>
        <dt>Last active</dt>
        <dd>{age(s.age_seconds)}</dd>
        <dt>Journal</dt>
        <dd>{s.events ?? 0} events</dd>
        <dt>Attached</dt>
        <dd>{s.attached ? 'yes' : 'no'}</dd>
        <dt>Retention</dt>
        <dd>{s.protected ? 'kept: it has an active goal or session plan' : 'removed once it passes [daemon] session_retention_days'}</dd>
      </dl>
      <p>
        {isProcess ? (
          <a href={`/processes/${encodeURIComponent(s.id)}`}>Open its process</a>
        ) : (
          <a href={`/conversations/${encodeURIComponent(s.id)}`}>Open the conversation</a>
        )}
      </p>
    </article>
  );
}

function Reflections() {
  const [agent, setAgent] = useState('self-reflection');
  const [asked, setAsked] = useState('self-reflection');
  const { data, error } = useLive(
    async () => {
      const r = await get(api.GET('/reflections', { params: { query: { agent: asked } } }), 'reflections');
      return { agent: r.agent, items: [...r.data].reverse() };
    },
    ['turn_end'],
    asked,
  );
  return (
    <section class={`card ${detail.detail}`} aria-labelledby="reflections-h">
      <header class={detail.head}>
        <h2 id="reflections-h" class={styles.title}>
          Reflections
        </h2>
      </header>
      <p class="muted">What a reflecting session concluded, newest first. Nine reflects on itself as self-reflection; a process may reflect in its own session.</p>
      <form
        class={styles.agent}
        onSubmit={(e) => {
          e.preventDefault();
          setAsked(agent.trim() || 'self-reflection');
        }}
      >
        <input class="input" aria-label="Reflecting session" value={agent} onInput={(e) => setAgent(e.currentTarget.value)} />
        <button type="submit" class="button">
          Show
        </button>
      </form>
      {error && <p class="error">{error}</p>}
      {!data && !error && <p class="muted">Loading…</p>}
      {data && data.items.length === 0 && <p class="empty">No reflections for {data.agent} yet.</p>}
      {data && data.items.length > 0 && (
        <ol class={styles.reflections}>
          {data.items.map((r, i) => (
            <li key={i}>
              <time class="muted" dateTime={r.at}>
                {new Date(r.at).toLocaleString()}
              </time>
              <Markdown text={r.text} class={styles.prose} />
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}

async function loadTopics(): Promise<{ docs: string[]; spec: string[] }> {
  const [d, s] = await Promise.all([get(api.GET('/docs'), 'the docs'), get(api.GET('/spec'), 'the spec')]);
  return { docs: [...d.topics].sort(), spec: [...s.topics].sort() };
}

function DocsView({ kind, topic }: { kind: DocKind; topic?: string }) {
  const { data: topics, error } = useLive(loadTopics, []);
  const [filter, setFilter] = useState('');
  const list = (topics?.[kind] ?? []).filter((t) => !filter || t.toLowerCase().includes(filter.trim().toLowerCase()));
  const label = kind === 'docs' ? 'Docs' : 'Spec';
  return (
    <Split name={kind} noun="topics" open={!!topic}>
      <nav class={split.list} aria-label={`${label} topics`}>
        <ListHead>
          <input
            class="input"
            type="search"
            aria-label={`Filter ${label.toLowerCase()} topics`}
            placeholder="Filter topics"
            value={filter}
            onInput={(e) => setFilter(e.currentTarget.value)}
          />
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!topics && !error && <p class="muted">Loading…</p>}
        {list.length > 0 && (
          <ul>
            {list.map((t) => (
              <li key={t}>
                <a href={topicPath(kind, t)} class={split.row} aria-current={t === topic ? 'page' : undefined}>
                  <span class={`${split.name} ${tools.wide}`}>{t}</span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Topic">
        {topic && topics ? (
          <Topic key={`${kind}/${topic}`} kind={kind} topic={topic} topics={topics} />
        ) : (
          <p class="empty">
            {kind === 'docs' ? 'The documentation this build of Nine embeds.' : 'The specification this build of Nine implements.'}
          </p>
        )}
      </section>
    </Split>
  );
}

function Topic({ kind, topic, topics }: { kind: DocKind; topic: string; topics: { docs: string[]; spec: string[] } }) {
  const { data, error } = useLive(async () => {
    const r =
      kind === 'docs'
        ? await api.GET('/docs/{topic}', { params: { path: { topic } } })
        : await api.GET('/spec/{topic}', { params: { path: { topic } } });
    if (r.response.status === 404) return null;
    if (!r.data) throw new Error(errorMessage(r.error, 'Could not load the topic.'));
    return r.data.content;
  }, []);
  if (error) return <p class="card error">{error}</p>;
  if (data === null) return <p class="empty">No topic {topic}.</p>;
  if (data === undefined) return <p class="card muted">Loading…</p>;
  return (
    <article class={`card ${styles.doc}`}>
      <a class={detail.back} href={`/system/${kind}`} aria-label="All topics">
        ‹
      </a>
      <Markdown text={data} class={styles.prose} link={(href) => docLink(kind, href, topics)} />
    </article>
  );
}
