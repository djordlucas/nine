import { useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get, type Schemas } from '../api/client';
import { duration } from '../format';
import { useLive } from '../live';
import { Markdown } from '../markdown';
import { argsTemplate, groupTools, kindLabels, parseArgs, type Tool } from '../tools';
import { Toggle } from '../components/Toggle';
import { Area } from '../components/Area';
import { ask } from '../components/Confirm';
import { ListHead, Split } from '../components/Split';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import { param } from './Processes';
import styles from './Tools.module.css';
import { Json } from '../components/Json';
import { Icon } from '../components/Icon';

type Plugin = Schemas['PluginInfo'];
type Skill = Schemas['SkillInfo'];

/** The Tools section's views. A tool cannot be named plugins or skills here: those paths are the other views. */
const views = [
  { path: '/tools', label: 'Tools' },
  { path: '/tools/plugins', label: 'Plugins' },
  { path: '/tools/skills', label: 'Skills' },
];

/**
 * Tools (adr/web-ui.md §5): the tool roster with provenance and
 * dependencies, one tool's detail with a direct call, versions and delete;
 * the plugins, with reload; and the skills, each readable.
 */
export function Tools() {
  const { path } = useLocation();
  const { params } = useRoute();
  const name = param(params.name as string | undefined);
  const view = path.startsWith('/tools/plugins') ? 'plugins' : path.startsWith('/tools/skills') ? 'skills' : 'tools';
  return (
    <div class={styles.section}>
      <nav class={styles.views} aria-label="Tools views">
        {views.map((v) => (
          <a
            key={v.path}
            href={v.path}
            class={styles.view}
            aria-current={(v.path === '/tools' ? view === 'tools' : path.startsWith(v.path)) ? 'page' : undefined}
          >
            {v.label}
          </a>
        ))}
      </nav>
      {view === 'tools' && <ToolsView name={name} />}
      {view === 'plugins' && <PluginsView />}
      {view === 'skills' && <SkillsView name={name} />}
    </div>
  );
}

function toolPath(name: string): string {
  return `/tools/${encodeURIComponent(name)}`;
}

async function loadTools(): Promise<Tool[]> {
  return (await get(api.GET('/tools', { params: { query: { limit: 1000 } } }), 'tools')).data ?? [];
}

function ToolsView({ name }: { name?: string }) {
  const [reloads, setReloads] = useState(0);
  const { data, error } = useLive(loadTools, [], reloads);
  return (
    <Split name="tools" noun="tools" open={!!name}>
      <ToolList tools={data} error={error} current={name} onReloaded={() => setReloads((n) => n + 1)} />
      <section class={split.pane} aria-label="Tool">
        {name ? (
          <ToolDetail key={name} name={name} onChanged={() => setReloads((n) => n + 1)} />
        ) : (
          <p class="empty">Choose a tool.</p>
        )}
      </section>
    </Split>
  );
}

function ToolList(props: { tools: Tool[] | null; error: string | null; current?: string; onReloaded: () => void }) {
  const [filter, setFilter] = useState('');
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<string | null>(null);
  const reload = async () => {
    setBusy(true);
    setOutcome(null);
    const { data, error } = await api.POST('/tools/reload');
    setBusy(false);
    setOutcome(data ? (data.message ?? 'Reloaded.') : errorMessage(error, 'Could not reload the tools.'));
    props.onReloaded();
  };
  const groups = props.tools ? groupTools(props.tools, filter) : [];
  return (
    <nav class={split.list} aria-label="Tools">
      <ListHead>
        <input
          class="input"
          type="search"
          aria-label="Filter tools"
          placeholder="Filter tools"
          value={filter}
          onInput={(e) => setFilter(e.currentTarget.value)}
        />
        <button type="button" class={`${detail.action} ${detail.iconAction}`} disabled={busy} onClick={() => void reload()} aria-label="Reload" title="Reload the tools">
          <Icon name="reload" size={16} />
        </button>
      </ListHead>
      {outcome && (
        <p class="muted" role="status">
          {outcome}
        </p>
      )}
      {props.error && <p class="error">{props.error}</p>}
      {!props.tools && !props.error && <p class="muted">Loading…</p>}
      {props.tools && groups.length === 0 && <p class="empty">No tool matches.</p>}
      {groups.map(([kind, tools]) => (
        <div key={kind} class={styles.group}>
          <h2 class={split.heading}>
            <span>{kindLabels[kind] ?? kind}</span>
            <span>{tools.length}</span>
          </h2>
          <ul>
            {tools.map((t) => (
              <li key={t.name}>
                <a href={toolPath(t.name ?? '')} class={split.row} aria-current={t.name === props.current ? 'page' : undefined}>
                  {t.loaded === false ? (
                    <span class={`${detail.badge} ${detail.failing}`}>failed</span>
                  ) : t.kind === 'plugin' && t.plugin ? (
                    <span class={detail.badge}>{t.plugin}</span>
                  ) : null}
                  <span class={`${split.name} ${t.loaded === false || t.kind === 'plugin' ? '' : styles.wide}`}>{t.name}</span>
                  {t.description && <span class={split.meta}>{t.description}</span>}
                </a>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </nav>
  );
}

function ToolDetail({ name, onChanged }: { name: string; onChanged: () => void }) {
  const { route } = useLocation();
  const [deleted, setDeleted] = useState(0);
  const { data: t, error } = useLive(
    async () => {
      const { data, error: e, response } = await api.GET('/tools/{name}', { params: { path: { name } } });
      if (response.status === 404) return null;
      if (!data) throw new Error(errorMessage(e, 'Could not load the tool.'));
      return data;
    },
    [],
    deleted,
  );
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);

  if (error) return <p class="card error">{error}</p>;
  if (t === null) return <p class="empty">No tool {name}. It may have been deleted.</p>;
  if (!t) return <p class="card muted">Loading…</p>;

  const remove = async () => {
    if (!(await ask({ title: `Delete ${name}?`, message: 'Its processes stop and it is gone from every session.', confirm: 'Delete', danger: true }))) return;
    const { data, error: e } = await api.DELETE('/tools/{name}', { params: { path: { name } } });
    if (data) {
      onChanged();
      setDeleted((n) => n + 1);
      route('/tools');
      return;
    }
    setOutcome({ ok: false, text: errorMessage(e, 'Could not delete the tool.') });
  };

  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href="/tools" aria-label="All tools">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{t.name}</h2>
          <span class="muted">{[kindLabels[t.kind ?? ''] ?? t.kind, t.plugin].filter(Boolean).join(' · ')}</span>
        </div>
        {t.generated && (
          <div class={detail.actions}>
            <button type="button" class={`${detail.action} ${detail.iconAction} ${detail.danger}`} onClick={() => void remove()} aria-label="Delete" title="Delete this tool">
              <Icon name="trash" size={16} />
            </button>
          </div>
        )}
      </header>
      {outcome && (
        <p class={outcome.ok ? detail.notice : 'error'} role="status">
          {outcome.text}
        </p>
      )}
      {t.loaded === false && <p class={`${detail.notice} error`}>It did not load. Fix it and reload the tools.</p>}
      {t.description && <p>{t.description}</p>}

      <dl class={detail.facts}>
        <dt>Capabilities</dt>
        <dd>
          {t.capabilities?.length ? (
            <ul class={styles.plain}>
              {t.capabilities.map((c) => (
                <li key={c}>
                  <code>{c}</code>
                </li>
              ))}
            </ul>
          ) : (
            'none'
          )}
        </dd>
        {t.deps?.length ? (
          <>
            <dt>Dependencies</dt>
            <dd>{t.deps.join(', ')}</dd>
          </>
        ) : null}
        {t.timeout && (
          <>
            <dt>Timeout</dt>
            <dd>{t.timeout}</dd>
          </>
        )}
        {t.manifest_path && (
          <>
            <dt>Manifest</dt>
            <dd>
              <code>{t.manifest_path}</code>
            </dd>
          </>
        )}
      </dl>

      {t.input_schema != null && (
        <details class={detail.section}>
          <summary>Arguments schema</summary>
          <Json value={t.input_schema} class={styles.code} />
        </details>
      )}
      {t.loaded !== false && <Call name={name} schema={t.input_schema} sandboxed={t.kind === 'sandboxed' || t.kind === 'generated'} />}
      {t.generated && <Versions name={name} onRestored={onChanged} />}
    </article>
  );
}

function Call({ name, schema, sandboxed }: { name: string; schema: unknown; sandboxed: boolean }) {
  const [text, setText] = useState(() => argsTemplate(schema));
  const [live, setLive] = useState(false);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; text: string; ms?: number } | null>(null);
  const call = async (e: Event) => {
    e.preventDefault();
    const parsed = parseArgs(text);
    if ('error' in parsed) {
      setResult({ ok: false, text: parsed.error });
      return;
    }
    setBusy(true);
    setResult(null);
    const { data, error } = await api.POST('/tools/{name}/call', {
      params: { path: { name } },
      body: sandboxed ? { args: parsed.args, live_state: live } : { args: parsed.args },
    });
    setBusy(false);
    setResult(
      data
        ? { ok: data.success !== false, text: data.output ?? '', ms: data.duration_ms }
        : { ok: false, text: errorMessage(error, 'The call failed.') },
    );
  };
  return (
    <section class={detail.section} aria-labelledby="call-h">
      <h3 id="call-h">Call it</h3>
      <p class="muted">Runs the tool now, as the operator, with these arguments. No model is involved.</p>
      <form class={styles.call} onSubmit={(e) => void call(e)}>
        <Area
          class={`input ${styles.code}`}
          rows={5}
          max={360}
          aria-label="Arguments as JSON"
          spellcheck={false}
          value={text}
          onInput={(e) => setText(e.currentTarget.value)}
        />
        <div class={styles.callRow}>
          {sandboxed && (
            <Toggle on={live} onChange={setLive}>
              Use its real state (a call otherwise gets a throwaway one)
            </Toggle>
          )}
          <button type="submit" class="button" disabled={busy}>
            {busy ? 'Calling…' : 'Call'}
          </button>
        </div>
      </form>
      {result && (
        <div role="status" class={styles.result}>
          <p class={result.ok ? 'muted' : 'error'}>
            {result.ok ? 'Returned' : 'Failed'}
            {result.ms !== undefined ? ` in ${duration(result.ms)}` : ''}
          </p>
          {result.text && <Json value={result.text} class={styles.code} />}
        </div>
      )}
    </section>
  );
}

function Versions({ name, onRestored }: { name: string; onRestored: () => void }) {
  const [restored, setRestored] = useState(0);
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  const { data, error } = useLive(
    async () => get(api.GET('/tools/{name}/versions', { params: { path: { name } } }), 'its versions'),
    [],
    restored,
  );
  const restore = async (version: number) => {
    if (!(await ask({ title: `Restore version ${version} of ${name}?`, message: 'It becomes current again. A process it runs restarts on it.', confirm: 'Restore' }))) return;
    const { data: done, error: e } = await api.POST('/tools/{name}/versions/{version}/restore', {
      params: { path: { name, version } },
    });
    setOutcome(done ? { ok: true, text: done.message ?? `Restored version ${version}.` } : { ok: false, text: errorMessage(e, 'Could not restore it.') });
    setRestored((n) => n + 1);
    onRestored();
  };
  return (
    <section class={detail.section} aria-labelledby="versions-h">
      <h3 id="versions-h">Versions</h3>
      {error && <p class="error">{error}</p>}
      {outcome && (
        <p class={outcome.ok ? 'muted' : 'error'} role="status">
          {outcome.text}
        </p>
      )}
      {data && (
        <>
          <p>
            Version {data.version}
            {data.written_by ? `, written by ${data.written_by}` : ''}
            {data.probation ? ' — on probation: rolled back if it fails before it first succeeds' : ''}.
          </p>
          {data.previous.length === 0 && <p class="empty">No earlier versions are kept.</p>}
          {data.previous.length > 0 && (
            <ul class={styles.versions}>
              {data.previous.map((v) => (
                <li key={v.version}>
                  <span>
                    Version {v.version}
                    <span class="muted">
                      {' '}
                      · {new Date(v.written_at).toLocaleString()}
                      {v.written_by ? ` · ${v.written_by}` : ''}
                      {v.lines ? ` · ${v.lines} lines` : ''}
                    </span>
                  </span>
                  <button type="button" class={detail.action} onClick={() => void restore(v.version)}>
                    Restore
                  </button>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}

function PluginsView() {
  const [reloads, setReloads] = useState(0);
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<string | null>(null);
  const { data, error } = useLive(
    async () => (await get(api.GET('/plugins', { params: { query: { limit: 1000 } } }), 'plugins')).data ?? [],
    [],
    reloads,
  );
  const reload = async () => {
    setBusy(true);
    setOutcome(null);
    const { data: done, error: e } = await api.POST('/plugins/reload');
    setBusy(false);
    setOutcome(done ? 'Plugins reloaded.' : errorMessage(e, 'Could not reload the plugins.'));
    setReloads((n) => n + 1);
  };
  return (
    <section class={`card ${detail.detail}`} aria-label="Plugins">
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>Plugins</h2>
          <span class="muted">Programs that serve tools. Reloading re-reads the plugin directories.</span>
        </div>
        <div class={detail.actions}>
          <button type="button" class={`${detail.action} ${detail.iconAction}`} disabled={busy} onClick={() => void reload()} aria-label="Reload" title="Reload the plugins">
            <Icon name="reload" size={16} />
          </button>
        </div>
      </header>
      {outcome && (
        <p class="muted" role="status">
          {outcome}
        </p>
      )}
      {error && <p class="error">{error}</p>}
      {!data && !error && <p class="muted">Loading…</p>}
      {data && data.length === 0 && <p class="empty">No plugins.</p>}
      {data && data.length > 0 && (
        <ul class={styles.plugins}>
          {data.map((p: Plugin) => (
            <li key={p.name}>
              <div class={styles.pluginHead}>
                <span class={`${detail.badge} ${p.disabled || p.loaded ? '' : detail.failing}`}>
                  {p.disabled ? 'disabled' : p.loaded ? 'loaded' : 'failed'}
                </span>
                <strong>{p.name}</strong>
                <span class="muted">{p.source}</span>
              </div>
              {p.error && <p class="error">{p.error}</p>}
              {p.tools && p.tools.length > 0 && (
                <p class={styles.pluginTools}>
                  {p.tools.map((t, i) => (
                    <span key={t}>
                      {i > 0 && ', '}
                      <a href={toolPath(t)}>{t}</a>
                    </span>
                  ))}
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function skillPath(name: string): string {
  return `/tools/skills/${encodeURIComponent(name)}`;
}

function SkillsView({ name }: { name?: string }) {
  const [filter, setFilter] = useState('');
  const { data, error } = useLive(
    async () => {
      const list = (await get(api.GET('/skills', { params: { query: { limit: 1000 } } }), 'skills')).data ?? [];
      return [...list].sort((a, b) => a.name.localeCompare(b.name));
    },
    [],
  );
  const f = filter.trim().toLowerCase();
  const shown = (data ?? []).filter(
    (s: Skill) => !f || `${s.name} ${s.description ?? ''} ${(s.tags ?? []).join(' ')}`.toLowerCase().includes(f),
  );
  return (
    <Split name="skills" noun="skills" open={!!name}>
      <nav class={split.list} aria-label="Skills">
        <ListHead>
          <input
            class="input"
            type="search"
            aria-label="Filter skills"
            placeholder="Filter skills"
            value={filter}
            onInput={(e) => setFilter(e.currentTarget.value)}
          />
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && shown.length === 0 && <p class="empty">No skill matches.</p>}
        {shown.length > 0 && (
          <ul>
            {shown.map((s) => (
              <li key={s.name}>
                <a href={skillPath(s.name)} class={split.row} aria-current={s.name === name ? 'page' : undefined}>
                  <span class={detail.badge}>{s.source}</span>
                  <span class={split.name}>{s.name}</span>
                  {s.description && <span class={split.meta}>{s.description}</span>}
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Skill">
        {name ? <SkillDetail key={name} name={name} /> : <p class="empty">Choose a skill to read it.</p>}
      </section>
    </Split>
  );
}

const sourceLabels: Record<string, string> = {
  builtin: 'shipped with Nine',
  user: 'from the skills directory',
  agent: 'written by Nine',
};

function SkillDetail({ name }: { name: string }) {
  const { data: s, error } = useLive(
    async () => {
      const { data, error: e, response } = await api.GET('/skills/{name}', { params: { path: { name } } });
      if (response.status === 404) return null;
      if (!data) throw new Error(errorMessage(e, 'Could not load the skill.'));
      return data;
    },
    [],
  );
  if (error) return <p class="card error">{error}</p>;
  if (s === null) return <p class="empty">No skill {name}.</p>;
  if (!s) return <p class="card muted">Loading…</p>;
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href="/tools/skills" aria-label="All skills">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{s.name}</h2>
          <span class="muted">
            {[sourceLabels[s.source] ?? s.source, ...(s.tags ?? [])].join(' · ')}
          </span>
        </div>
      </header>
      {s.description && <p class="muted">{s.description}</p>}
      <Markdown text={s.content} />
    </article>
  );
}
