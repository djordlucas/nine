import { useEffect, useRef, useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get, type Schemas } from '../api/client';
import { ago } from '../format';
import { copyText } from '../clipboard';
import { useLive } from '../live';
import { bytes } from '../system';
import { ask } from '../components/Confirm';
import { Area } from '../components/Area';
import { Toggle } from '../components/Toggle';
import { Icon } from '../components/Icon';
import { ListHead, Split } from '../components/Split';
import { Blank } from '../components/Blank';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import { param } from './Processes';
import { PluginsView, ToolsView } from './Tools';
import tabs from '../components/Tabs.module.css';
import styles from './Builder.module.css';

type Agent = Schemas['Agent'];
type AgentDefinition = Schemas['AgentDefinition'];
type RoleInfo = Schemas['RoleInfo'];
type SkillInfo = Schemas['SkillInfo'];
type MemoryEntry = Schemas['MemoryListResponse']['entries'][number];

const views = [
  { path: '/builder/agents', label: 'Agents' },
  { path: '/builder/roles', label: 'Roles' },
  { path: '/builder/skills', label: 'Skills' },
  { path: '/builder/data', label: 'Data' },
  // Plugins before Tools: /builder/tools is a prefix of /builder/tools/plugins.
  { path: '/builder/tools/plugins', label: 'Plugins' },
  { path: '/builder/tools', label: 'Tools' },
];

/**
 * Builder (adr/agents-concept.md A8, Q24/Q25): where the operator composes
 * agents, roles, skills and the key-value memory over REST — the same stores
 * the model's tools write (Q23). Agents run into a conversation, export as a
 * bundle tar, and import staged until approved (Q30).
 */
export function Builder() {
  const { path, query } = useLocation();
  const { params } = useRoute();
  const view = views.find((v) => path === v.path || path.startsWith(v.path + '/'))?.path ?? '/builder/agents';
  return (
    <div class={tabs.section}>
      <nav class={tabs.views} aria-label="Builder views">
        {views.map((v) => (
          <a key={v.path} href={v.path} class={tabs.view} aria-current={v.path === view ? 'page' : undefined}>
            {v.label}
          </a>
        ))}
      </nav>
      {view === '/builder/agents' && <AgentsView id={param(params.id as string | undefined)} />}
      {view === '/builder/roles' && <RolesView name={param(params.name as string | undefined)} />}
      {view === '/builder/skills' && <SkillsView name={param(params.name as string | undefined)} />}
      {view === '/builder/data' && <DataView memoryKey={query.key ?? (path === '/builder/data/new' ? 'new' : undefined)} />}
      {view === '/builder/tools/plugins' && <PluginsView />}
      {view === '/builder/tools' && <ToolsView name={param(params.name as string | undefined)} />}
    </div>
  );
}

function agentHref(id: string): string {
  return `/builder/agents/${encodeURIComponent(id)}`;
}

function roleHref(name: string): string {
  return `/builder/roles/${encodeURIComponent(name)}`;
}

function skillHref(name: string): string {
  return `/builder/skills/${encodeURIComponent(name)}`;
}

function dataHref(key: string): string {
  return `/builder/data?key=${encodeURIComponent(key)}`;
}



/** Reads a File as a base64 string, in chunks (String.fromCharCode overflows on large buffers). */
async function fileBase64(f: File): Promise<string> {
  const buf = new Uint8Array(await f.arrayBuffer());
  let bin = '';
  for (let i = 0; i < buf.length; i += 0x8000) bin += String.fromCharCode(...buf.subarray(i, i + 0x8000));
  return btoa(bin);
}

function base64Bytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const buf = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
  return buf;
}

function download(name: string, content: Uint8Array, type: string) {
  const url = URL.createObjectURL(new Blob([content as BlobPart], { type }));
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

// --- Agents ---

function AgentsView({ id }: { id?: string }) {
  const { route } = useLocation();
  const [reloads, setReloads] = useState(0);
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<string | null>(null);
  const [filter, setFilter] = useState('');
  const fileInput = useRef<HTMLInputElement>(null);
  const { data, error } = useLive(
    async () => {
      const agents = (await get(api.GET('/agents', { params: { query: { limit: 1000 } } }), 'agents')).data ?? [];
      const roles = (await get(api.GET('/roles'), 'roles')).data ?? [];
      const skills = (await get(api.GET('/skills', { params: { query: { limit: 1000 } } }), 'skills')).data ?? [];
      return {
        agents: [...agents].sort((a, b) => a.definition.name.localeCompare(b.definition.name)),
        roles: [...roles].sort((a, b) => a.name.localeCompare(b.name)),
        skills: [...skills].sort((a, b) => a.name.localeCompare(b.name)),
      };
    },
    [],
    reloads,
  );

  // Import an exported bundle (adr/agents-concept.md Q26/Q30): the tar goes up
  // base64 and lands staged until approved.
  const importBundle = async (files: FileList | null) => {
    if (!files || files.length === 0) return;
    setBusy(true);
    setOutcome(null);
    const { data: done, error: e } = await api.POST('/agents/import', { body: { tar_base64: await fileBase64(files[0]) } });
    setBusy(false);
    setOutcome(done ? `Imported ${done.definition.name} as ${done.status}${done.status === 'staged' ? ' — approve it below' : ''}.` : errorMessage(e, 'Could not import the agent.'));
    setReloads((n) => n + 1);
  };

  const run = async (agentId: string) => {
    setBusy(true);
    setOutcome(null);
    const { data: r, error: e } = await api.POST('/agents/{id}/run', { params: { path: { id: agentId } }, body: { interactive: false } });
    setBusy(false);
    if (r) route(`/conversations/${encodeURIComponent(r.conversation_id)}`);
    else setOutcome(errorMessage(e, 'Could not run the agent.'));
  };

  const exportBundle = async (agentId: string) => {
    setOutcome(null);
    const { data: ex, error: e } = await api.GET('/agents/{id}/export', { params: { path: { id: agentId } } });
    if (!ex) {
      setOutcome(errorMessage(e, 'Could not export the agent.'));
      return;
    }
    download(ex.filename, base64Bytes(ex.tar_base64), 'application/gzip');
  };

  const approve = async (agentId: string) => {
    setBusy(true);
    setOutcome(null);
    const { data: done, error: e } = await api.POST('/agents/{id}/approve', { params: { path: { id: agentId } } });
    setBusy(false);
    setOutcome(done ? `Approved ${done.definition.name}.` : errorMessage(e, 'Could not approve the agent.'));
    setReloads((n) => n + 1);
  };

  const remove = async (agent: Agent) => {
    if (!(await ask({ title: `Delete ${agent.definition.name}?`, message: 'The definition and its memory database are removed.', confirm: 'Delete', danger: true }))) return;
    setBusy(true);
    setOutcome(null);
    const { data: done, error: e } = await api.DELETE('/agents/{id}', { params: { path: { id: agent.id } } });
    setBusy(false);
    setOutcome(done ? `Deleted ${done.id}.` : errorMessage(e, 'Could not delete the agent.'));
    if (done) route('/builder/agents');
    setReloads((n) => n + 1);
  };

  const f = filter.trim().toLowerCase();
  const shown = (data?.agents ?? []).filter((a) => !f || `${a.definition.name} ${a.definition.role ?? ''}`.toLowerCase().includes(f));
  return (
    <Split name="builder-agents" noun="agents" open={!!id}>
      <nav class={split.list} aria-label="Agents">
        <ListHead>
          <input class="input" type="search" aria-label="Filter agents" placeholder="Filter agents" value={filter} onInput={(e) => setFilter(e.currentTarget.value)} />
          <a class="button" href={agentHref('new')}>
            New agent
          </a>
          <input
            ref={fileInput}
            type="file"
            accept=".tar,.tar.gz,.tgz"
            hidden
            onChange={(e) => {
              void importBundle((e.target as HTMLInputElement).files);
              (e.target as HTMLInputElement).value = '';
            }}
          />
          <button type="button" class={`${detail.action} ${detail.secondary}`} disabled={busy} onClick={() => fileInput.current?.click()} title="Import an exported agent bundle (lands staged until approved)">
            <Icon name="upload" size={16} /> Import
          </button>
          {data && (
            <span class="muted">
              {data.agents.length} agent{data.agents.length === 1 ? '' : 's'}
            </span>
          )}
        </ListHead>
        {outcome && (
          <p class="muted" role="status">
            {outcome}
          </p>
        )}
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && shown.length === 0 && <p class="empty">{data.agents.length === 0 ? 'No agents yet. Create one, or import a bundle.' : 'No agent matches.'}</p>}
        {shown.length > 0 && (
          <ul>
            {shown.map((a) => (
              <li key={a.id}>
                <a href={agentHref(a.id)} class={styles.row} aria-current={a.id === id ? 'page' : undefined}>
                  <span class={styles.nameLine}>
                    <span class={styles.dot} data-state={a.status} title={a.status} aria-hidden="true" />
                    <span class={styles.name}>{a.definition.name}</span>
                    {a.updated_at && <span class={styles.age}>{ago(a.updated_at)}</span>}
                  </span>
                  <span class={styles.meta}>
                    {a.status === 'staged' && <span className={`${detail.badge} ${detail.failing}`}>staged</span>}
                    {[a.definition.role, `${(a.definition.skills ?? []).length} skills`, `v${a.version}`].filter(Boolean).join(' · ')}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Agent">
        {!data ? (
          <p class="empty">Loading…</p>
        ) : id ? (
          <AgentForm key={id} id={id} roles={data.roles} skills={data.skills} reload={reloads} onSaved={() => setReloads((n) => n + 1)} onRun={run} onExport={exportBundle} onApprove={approve} onDelete={remove} busy={busy} />
        ) : (
          <Blank title="Agents">
            An agent is a named definition — a role, skills, an optional self-model — with its own memory database. Create one here or import an exported bundle; imports land staged until approved, and Run opens a new conversation.
          </Blank>
        )}
      </section>
    </Split>
  );
}

function AgentForm({
  id,
  roles,
  skills,
  reload,
  onSaved,
  onRun,
  onExport,
  onApprove,
  onDelete,
  busy,
}: {
  id: string;
  roles: RoleInfo[];
  skills: SkillInfo[];
  reload: number;
  onSaved: () => void;
  onRun: (id: string) => void;
  onExport: (id: string) => void;
  onApprove: (id: string) => void;
  onDelete: (a: Agent) => void;
  busy: boolean;
}) {
  const { route } = useLocation();
  const isNew = id === 'new';
  const [agent, setAgent] = useState<Agent | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [name, setName] = useState('');
  const [role, setRole] = useState('');
  const [skillsText, setSkillsText] = useState('');
  const [selfModel, setSelfModel] = useState('');
  const [reflection, setReflection] = useState('');
  const [busySave, setBusySave] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The loaded values, to tell "unsaved changes" from a saved form.
  const savedRef = useRef<{ name: string; role: string; skillsText: string; selfModel: string; reflection: string } | null>(null);

  useEffect(() => {
    if (isNew) return;
    let alive = true;
    void (async () => {
      const { data: a, error: e } = await api.GET('/agents/{id}', { params: { path: { id } } });
      if (!alive) return;
      if (!a) {
        setLoadError(errorMessage(e, 'Could not load the agent.'));
        return;
      }
      const loaded = {
        name: a.definition.name,
        role: a.definition.role ?? '',
        skillsText: (a.definition.skills ?? []).join(', '),
        selfModel: a.definition.self_model ? JSON.stringify(a.definition.self_model, null, 2) : '',
        reflection: a.definition.reflection ? JSON.stringify(a.definition.reflection, null, 2) : '',
      };
      const prev = savedRef.current;
      setAgent(a);
      // A reload (after a save) must not clobber fields the operator is
      // editing: each field takes the server's value only if it still holds
      // the previously loaded one (the updater reads the latest state, so a
      // concurrent edit wins over the incoming load).
      setName((cur) => (prev && cur !== prev.name ? cur : loaded.name));
      setRole((cur) => (prev && cur !== prev.role ? cur : loaded.role));
      setSkillsText((cur) => (prev && cur !== prev.skillsText ? cur : loaded.skillsText));
      setSelfModel((cur) => (prev && cur !== prev.selfModel ? cur : loaded.selfModel));
      setReflection((cur) => (prev && cur !== prev.reflection ? cur : loaded.reflection));
      savedRef.current = loaded;
    })();
    return () => {
      alive = false;
    };
  }, [id, isNew, reload]);

  const dirty =
    !isNew &&
    savedRef.current !== null &&
    (name !== savedRef.current.name ||
      role !== savedRef.current.role ||
      skillsText !== savedRef.current.skillsText ||
      selfModel !== savedRef.current.selfModel ||
      reflection !== savedRef.current.reflection);
  const selectedSkills = skillsText
    .split(',')
    .map((x) => x.trim())
    .filter(Boolean);
  // Chips cover the registered skills plus any the definition already names
  // (a skill since deleted from the store still shows, so it can be removed).
  const skillNames = [...new Set([...skills.map((x) => x.name), ...selectedSkills])].sort((a, b) => a.localeCompare(b));
  const toggleSkill = (n: string) => {
    const next = selectedSkills.includes(n) ? selectedSkills.filter((x) => x !== n) : [...selectedSkills, n];
    setSkillsText(next.join(', '));
  };

  const parseJson = (text: string): Record<string, unknown> | undefined => {
    const t = text.trim();
    if (!t) return undefined;
    return JSON.parse(t) as Record<string, unknown>;
  };

  const save = async () => {
    setBusySave(true);
    setError(null);
    try {
      const def: AgentDefinition = {
        name: name.trim(),
        role: role || undefined,
        skills: skillsText
          .split(',')
          .map((s) => s.trim())
          .filter(Boolean),
        self_model: parseJson(selfModel),
        reflection: parseJson(reflection),
      };
      if (isNew) {
        const { data: a, error: e } = await api.POST('/agents', { body: def });
        if (!a) throw new Error(errorMessage(e, 'Could not create the agent.'));
        route(agentHref(a.id));
      } else {
        const { data: a, error: e } = await api.PATCH('/agents/{id}', { params: { path: { id } }, body: def });
        if (!a) throw new Error(errorMessage(e, 'Could not update the agent.'));
      }
      savedRef.current = { name: name.trim(), role, skillsText, selfModel, reflection };
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusySave(false);
    }
  };

  if (loadError) return <p class="card error">{loadError}</p>;
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>{isNew ? 'New agent' : name || id}</h2>
          {agent && (
            <span class="muted">
              {agent.id} · v{agent.version} · {agent.status}
            </span>
          )}
          {dirty && <span class="muted">Unsaved changes</span>}
        </div>
        {!isNew && agent && (
          <div class={detail.actions}>
            <button
              type="button"
              class={`${detail.action} ${detail.iconAction}`}
              aria-label="Copy link"
              title="Copy a link to this agent"
              onClick={() => void copyText(`${window.location.origin}${agentHref(agent.id)}`)}
            >
              <Icon name="link" size={16} />
            </button>
            {agent.status === 'staged' && (
              <button type="button" class={`${detail.action} ${detail.primary}`} disabled={busy} onClick={() => onApprove(agent.id)}>
                Approve
              </button>
            )}
            <button type="button" class={detail.action} disabled={busy || agent.status === 'staged'} title={agent.status === 'staged' ? 'Approve the import before running' : 'Run the agent in a new conversation'} onClick={() => onRun(agent.id)}>
              <Icon name="play" size={16} /> Run
            </button>
            <button type="button" class={detail.action} disabled={busy} onClick={() => onExport(agent.id)} title="Download the agent bundle (definition plus its memory database)">
              <Icon name="download" size={16} /> Export
            </button>
            <button type="button" class={`${detail.action} ${detail.danger}`} disabled={busy} onClick={() => onDelete(agent)}>
              Delete
            </button>
          </div>
        )}
      </header>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      <form
        class={styles.form}
        aria-label="Agent definition"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <div class={styles.formRow}>
          <div class={styles.field}>
            <label for="agent-name">Name</label>
            <input id="agent-name" class="input" value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} placeholder="researcher" required />
          </div>
          <div class={styles.field}>
            <label for="agent-role">Role</label>
            <select id="agent-role" class="input" value={role} onChange={(e) => setRole((e.target as HTMLSelectElement).value)}>
              <option value="">default role at run time</option>
              {roles.map((r) => (
                <option key={r.name} value={r.name}>
                  {r.name}
                </option>
              ))}
            </select>
          </div>
        </div>
        <div class={styles.field}>
          <span class={styles.fieldLabel} id="agent-skills-label">
            Skills
          </span>
          {skillNames.length > 0 ? (
            <div class={styles.chips} role="group" aria-labelledby="agent-skills-label">
              {skillNames.map((n) => (
                <button type="button" key={n} class={styles.chip} aria-pressed={selectedSkills.includes(n)} onClick={() => toggleSkill(n)}>
                  {n}
                </button>
              ))}
            </div>
          ) : (
            <p class="muted">No skills yet — create some in the Skills view, then attach them here.</p>
          )}
        </div>
        <div class={styles.field}>
          <label for="agent-self-model">Self-model (JSON, optional)</label>
          <Area id="agent-self-model" class="input" rows={4} max={260} value={selfModel} onInput={(e) => setSelfModel((e.target as HTMLTextAreaElement).value)} />
        </div>
        <div class={styles.field}>
          <label for="agent-reflection">Reflection (JSON, optional)</label>
          <Area id="agent-reflection" class="input" rows={4} max={260} value={reflection} onInput={(e) => setReflection((e.target as HTMLTextAreaElement).value)} />
        </div>
        <div class={detail.actions}>
          <button class={`${detail.action} ${detail.primary}`} type="submit" disabled={busySave || !name.trim()}>
            {busySave ? 'Saving…' : isNew ? 'Create' : 'Save'}
          </button>
        </div>
      </form>
    </article>
  );
}

// --- Roles ---

function RolesView({ name }: { name?: string }) {
  const { route } = useLocation();
  const [reloads, setReloads] = useState(0);
  const [filter, setFilter] = useState('');
  const { data, error } = useLive(async () => {
    const roles = (await get(api.GET('/roles'), 'roles')).data ?? [];
    // Tool names, for the allowlist's autocomplete and unknown-name warning.
    const tools = (await get(api.GET('/tools', { params: { query: { limit: 1000 } } }), 'tools')).data ?? [];
    return {
      roles: [...roles].sort((a, b) => a.name.localeCompare(b.name)),
      toolNames: [...new Set(tools.map((t) => t.name).filter((n): n is string => !!n))].sort((a, b) => a.localeCompare(b)),
    };
  }, [], reloads);
  const f = filter.trim().toLowerCase();
  const shown = (data?.roles ?? []).filter((r) => !f || `${r.name} ${r.description ?? ''}`.toLowerCase().includes(f));

  const remove = async (roleName: string) => {
    if (!(await ask({ title: `Delete role ${roleName}?`, message: 'The role skill is removed; conversations that asked for it fall back to the default role.', confirm: 'Delete', danger: true }))) return;
    const { error: e } = await api.DELETE('/roles/{name}', { params: { path: { name: roleName } } });
    if (e) return; // shown by the form's outcome on reload
    route('/builder/roles');
    setReloads((n) => n + 1);
  };

  return (
    <Split name="builder-roles" noun="roles" open={!!name}>
      <nav class={split.list} aria-label="Roles">
        <ListHead>
          <input class="input" type="search" aria-label="Filter roles" placeholder="Filter roles" value={filter} onInput={(e) => setFilter(e.currentTarget.value)} />
          <a class="button" href={roleHref('new')}>
            New role
          </a>
          {data && (
            <span class="muted">
              {data.roles.length} role{data.roles.length === 1 ? '' : 's'}
            </span>
          )}
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && shown.length === 0 && <p class="empty">{data.roles.length === 0 ? 'No roles.' : 'No role matches.'}</p>}
        {shown.length > 0 && (
          <ul>
            {shown.map((r) => (
              <li key={r.name}>
                <a href={roleHref(r.name)} class={styles.row} aria-current={r.name === name ? 'page' : undefined}>
                  <span class={styles.nameLine}>
                    <span class={styles.name}>{r.name}</span>
                  </span>
                  <span class={styles.meta}>
                    <span class={detail.badge}>{r.all_tools ? 'all tools' : `${r.tools?.length ?? 0} tools`}</span>
                    {[r.leaf ? 'leaf' : null, r.delegates ? 'delegates' : null, r.description].filter(Boolean).join(' · ')}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Role">
        {data && (name ? <RoleForm key={name} name={name} roles={data.roles} toolNames={data.toolNames} onSaved={() => setReloads((n) => n + 1)} onDelete={remove} /> : (
          <Blank title="Roles">
            A role is a persona with a tool allowlist, stored as a skill with a role block. Pick one to edit it, or start from New role.
          </Blank>
        ))}
      </section>
    </Split>
  );
}

function RoleForm({ name, roles, toolNames, onSaved, onDelete }: { name: string; roles: RoleInfo[]; toolNames: string[]; onSaved: () => void; onDelete: (name: string) => void }) {
  const isNew = name === 'new';
  const existing = roles.find((r) => r.name === name);
  const [roleName, setRoleName] = useState(existing?.name ?? '');
  const [description, setDescription] = useState(existing?.description ?? '');
  const [allTools, setAllTools] = useState(existing?.all_tools ?? false);
  const [toolsText, setToolsText] = useState((existing?.tools ?? []).join(', '));
  const [delegates, setDelegates] = useState(existing?.delegates ?? false);
  const [spawnsGoals, setSpawnsGoals] = useState(existing?.spawns_goals ?? false);
  const [persists, setPersists] = useState(existing?.persists ?? false);
  const [interactive, setInteractive] = useState(existing?.interactive ?? false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The structural toggles do not always round-trip (the daemon strips them
  // for agent-authored roles), so the dirty flag reads only what does.
  const dirty =
    !isNew &&
    !!existing &&
    (description !== (existing.description ?? '') || allTools !== existing.all_tools || (!allTools && toolsText !== (existing.tools ?? []).join(', ')));
  const enteredTools = toolsText
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean);
  const unknownTools = enteredTools.filter((t) => !toolNames.includes(t));

  const save = async () => {
    setBusy(true);
    setError(null);
    const tools = enteredTools;
    try {
      if (isNew) {
        const { error: e } = await api.POST('/roles', {
          body: { name: roleName.trim(), description: description || undefined, all_tools: allTools, tools, delegates, spawns_goals: spawnsGoals, persists, interactive },
        });
        if (e) throw new Error(errorMessage(e, 'Could not create the role.'));
      } else {
        const { error: e } = await api.PATCH('/roles/{name}', {
          params: { path: { name } },
          body: { description: description || undefined, all_tools: allTools, tools, delegates, spawns_goals: spawnsGoals, persists, interactive },
        });
        if (e) throw new Error(errorMessage(e, 'Could not update the role.'));
      }
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>{isNew ? 'New role' : name}</h2>
          {existing?.leaf && <span class="muted">delegatable leaf</span>}
          {dirty && <span class="muted">Unsaved changes</span>}
        </div>
        {!isNew && (
          <div class={detail.actions}>
            <button type="button" class={`${detail.action} ${detail.danger}`} disabled={busy} onClick={() => onDelete(name)}>
              Delete
            </button>
          </div>
        )}
      </header>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      <form
        class={styles.form}
        aria-label="Role"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <div class={styles.field}>
          <label for="role-name">Name</label>
          <input id="role-name" class="input" value={roleName} onInput={(e) => setRoleName((e.target as HTMLInputElement).value)} placeholder="coder" required disabled={!isNew} />
        </div>
        <div class={styles.field}>
          <label for="role-description">Description (the role's persona)</label>
          <Area id="role-description" class="input" rows={2} max={160} value={description} onInput={(e) => setDescription((e.target as HTMLTextAreaElement).value)} />
        </div>
        <div class={styles.field}>
          <Toggle on={allTools} onChange={setAllTools}>
            All tools
          </Toggle>
          {!allTools && (
            <>
              <label for="role-tools">Tool allowlist (comma-separated)</label>
              <input
                id="role-tools"
                class="input"
                list="role-tool-names"
                value={toolsText}
                onInput={(e) => setToolsText((e.target as HTMLInputElement).value)}
                placeholder="shell, file_read, file_write"
              />
              <datalist id="role-tool-names">
                {toolNames.map((n) => (
                  <option key={n} value={n} />
                ))}
              </datalist>
              {unknownTools.length > 0 && (
                <p class="muted" role="alert">
                  Unknown tools: {unknownTools.join(', ')}
                </p>
              )}
            </>
          )}
        </div>
        <div class={styles.field}>
          <Toggle on={delegates} onChange={setDelegates}>
            Delegates (may spawn sub-agents)
          </Toggle>
          <Toggle on={spawnsGoals} onChange={setSpawnsGoals}>
            Spawns goals (background pursue sessions)
          </Toggle>
          <Toggle on={persists} onChange={setPersists}>
            Persists (checkpointing session worker)
          </Toggle>
          <Toggle on={interactive} onChange={setInteractive}>
            Interactive (HITL-eligible)
          </Toggle>
        </div>
        <div class={detail.actions}>
          <button class={`${detail.action} ${detail.primary}`} type="submit" disabled={busy || !roleName.trim() || (!allTools && toolsText.trim() === '' && !isNew && (existing?.tools ?? []).length === 0)}>
            {busy ? 'Saving…' : isNew ? 'Create' : 'Save'}
          </button>
        </div>
      </form>
    </article>
  );
}

// --- Skills ---

function SkillsView({ name }: { name?: string }) {
  const { route } = useLocation();
  const [reloads, setReloads] = useState(0);
  const [filter, setFilter] = useState('');
  const { data, error } = useLive(async () => {
    const list = (await get(api.GET('/skills', { params: { query: { limit: 1000 } } }), 'skills')).data ?? [];
    return [...list].sort((a, b) => a.name.localeCompare(b.name));
  }, [], reloads);
  const f = filter.trim().toLowerCase();
  const shown = (data ?? []).filter((s) => !f || `${s.name} ${s.description ?? ''} ${(s.tags ?? []).join(' ')}`.toLowerCase().includes(f));

  const remove = async (skillName: string) => {
    if (!(await ask({ title: `Delete skill ${skillName}?`, message: 'The skill is removed from the store.', confirm: 'Delete', danger: true }))) return;
    const { error: e } = await api.DELETE('/skills/{name}', { params: { path: { name: skillName } } });
    if (e) return;
    route('/builder/skills');
    setReloads((n) => n + 1);
  };

  return (
    <Split name="builder-skills" noun="skills" open={!!name}>
      <nav class={split.list} aria-label="Skills">
        <ListHead>
          <input class="input" type="search" aria-label="Filter skills" placeholder="Filter skills" value={filter} onInput={(e) => setFilter(e.currentTarget.value)} />
          <a class="button" href={skillHref('new')}>
            New skill
          </a>
          {data && (
            <span class="muted">
              {data.length} skill{data.length === 1 ? '' : 's'}
            </span>
          )}
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && shown.length === 0 && <p class="empty">No skill matches.</p>}
        {shown.length > 0 && (
          <ul>
            {shown.map((s) => (
              <li key={s.name}>
                <a href={skillHref(s.name)} class={styles.row} aria-current={s.name === name ? 'page' : undefined}>
                  <span class={styles.nameLine}>
                    <span class={styles.name}>{s.name}</span>
                  </span>
                  <span class={styles.meta}>
                    <span class={detail.badge}>{s.source}</span>
                    {s.description}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Skill">
        {data && (name ? <SkillForm key={name} name={name} onSaved={() => setReloads((n) => n + 1)} onDelete={remove} /> : (
          <Blank title="Skills">
            Skills are Markdown programs the model can read and call. Agent-written ones are editable here; builtin and user skills are read-only.
          </Blank>
        ))}
      </section>
    </Split>
  );
}

function SkillForm({ name, onSaved, onDelete }: { name: string; onSaved: () => void; onDelete: (name: string) => void }) {
  const { route } = useLocation();
  const isNew = name === 'new';
  const [source, setSource] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [skillName, setSkillName] = useState('');
  const [description, setDescription] = useState('');
  const [tagsText, setTagsText] = useState('');
  const [content, setContent] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Only agent-written skills are editable through REST; builtin and user
  // skills are read-only (the daemon refuses the write).
  const writable = isNew || source === 'agent';
  // The loaded values, to tell "unsaved changes" from a saved form.
  const savedRef = useRef<{ description: string; tagsText: string; content: string } | null>(null);

  useEffect(() => {
    if (isNew) return;
    let alive = true;
    void (async () => {
      const { data: s, error: e } = await api.GET('/skills/{name}', { params: { path: { name } } });
      if (!alive) return;
      if (!s) {
        setLoadError(errorMessage(e, 'Could not load the skill.'));
        return;
      }
      const loaded = { description: s.description ?? '', tagsText: (s.tags ?? []).join(', '), content: s.content };
      savedRef.current = loaded;
      setSource(s.source);
      setSkillName(s.name);
      setDescription(loaded.description);
      setTagsText(loaded.tagsText);
      setContent(loaded.content);
    })();
    return () => {
      alive = false;
    };
  }, [name, isNew]);

  const save = async () => {
    setBusy(true);
    setError(null);
    const tags = tagsText
      .split(',')
      .map((t) => t.trim())
      .filter(Boolean);
    try {
      if (isNew) {
        const { data: s, error: e } = await api.POST('/skills', { body: { name: skillName.trim(), description: description || undefined, tags, content } });
        if (!s) throw new Error(errorMessage(e, 'Could not create the skill.'));
        route(skillHref(s.name));
      } else {
        const { error: e } = await api.PATCH('/skills/{name}', { params: { path: { name } }, body: { description: description || undefined, tags, content } });
        if (e) throw new Error(errorMessage(e, 'Could not update the skill.'));
      }
      savedRef.current = { description, tagsText, content };
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const dirty =
    !isNew &&
    writable &&
    savedRef.current !== null &&
    (description !== savedRef.current.description || tagsText !== savedRef.current.tagsText || content !== savedRef.current.content);

  if (loadError) return <p class="card error">{loadError}</p>;
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>{isNew ? 'New skill' : name}</h2>
          {source && <span class={detail.badge}>{source}</span>}
          {dirty && <span class="muted">Unsaved changes</span>}
        </div>
        {!isNew && (
          <div class={detail.actions}>
            {writable && (
              <button type="button" class={`${detail.action} ${detail.danger}`} disabled={busy} onClick={() => onDelete(name)}>
                Delete
              </button>
            )}
          </div>
        )}
      </header>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      {!writable && !isNew && <p class="muted">Builtin and user skills are read-only here; only agent-written skills can be edited or deleted.</p>}
      <form
        class={styles.form}
        aria-label="Skill"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <div class={styles.field}>
          <label for="skill-name">Name</label>
          <input id="skill-name" class="input" value={skillName} onInput={(e) => setSkillName((e.target as HTMLInputElement).value)} placeholder="code-review" required disabled={!isNew} />
        </div>
        <div class={styles.field}>
          <label for="skill-description">Description</label>
          <input id="skill-description" class="input" value={description} onInput={(e) => setDescription((e.target as HTMLInputElement).value)} disabled={!writable} />
        </div>
        <div class={styles.field}>
          <label for="skill-tags">Tags (comma-separated)</label>
          <input id="skill-tags" class="input" value={tagsText} onInput={(e) => setTagsText((e.target as HTMLInputElement).value)} disabled={!writable} />
        </div>
        <div class={styles.field}>
          <label for="skill-content">Body (Markdown)</label>
          <Area id="skill-content" class="input" rows={12} max={480} value={content} onInput={(e) => setContent((e.target as HTMLTextAreaElement).value)} disabled={!writable} />
        </div>
        {writable && (
          <div class={detail.actions}>
            <button class={`${detail.action} ${detail.primary}`} type="submit" disabled={busy || !skillName.trim() || !content.trim()}>
              {busy ? 'Saving…' : isNew ? 'Create' : 'Save'}
            </button>
          </div>
        )}
      </form>
    </article>
  );
}

// --- Data: the key-value memory ---

function DataView({ memoryKey }: { memoryKey?: string }) {
  const { route } = useLocation();
  const [reloads, setReloads] = useState(0);
  const [filter, setFilter] = useState('');
  const { data, error } = useLive(async () => (await get(api.GET('/memory'), 'the memory')).entries, ['turn_end'], reloads);
  const f = filter.trim().toLowerCase();
  const shown = (data ?? []).filter((e) => !f || e.key.toLowerCase().includes(f));

  const remove = async (key: string) => {
    if (!(await ask({ title: `Delete ${key}?`, message: 'The key is removed from memory.', confirm: 'Delete', danger: true }))) return;
    const { error: e } = await api.DELETE('/memory/value', { params: { query: { key } } });
    if (e) return;
    route('/builder/data');
    setReloads((n) => n + 1);
  };

  // '/builder/data/new' is the NewKey form — a path, not a ?key=, so a phone
  // (which shows the list or the pane, never both) can reach it from the list.
  const isNew = memoryKey === 'new';
  return (
    <Split name="builder-data" noun="memory keys" open={memoryKey !== undefined}>
      <nav class={split.list} aria-label="Memory keys">
        <ListHead>
          <input class="input" type="search" aria-label="Filter memory keys" placeholder="Filter keys" value={filter} onInput={(e) => setFilter(e.currentTarget.value)} />
          <a class="button" href="/builder/data/new">
            New key
          </a>
          {data && (
            <span class="muted">
              {data.length} key{data.length === 1 ? '' : 's'}
            </span>
          )}
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && shown.length === 0 && <p class="empty">{data.length ? 'No key matches.' : 'Nothing in memory yet.'}</p>}
        {shown.length > 0 && (
          <ul>
            {shown.map((e: MemoryEntry) => (
              <li key={e.key}>
                <a href={dataHref(e.key)} class={styles.row} aria-current={e.key === memoryKey ? 'page' : undefined}>
                  <span class={styles.nameLine}>
                    <span class={styles.name}>{e.key}</span>
                    {e.updated_at && <span class={styles.age}>{ago(e.updated_at)}</span>}
                  </span>
                  <span class={styles.meta}>{bytes(e.bytes)}</span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Memory key">
        {memoryKey === undefined ? (
          <Blank title="Data">
            The key-value memory every agent reads and writes through memory_get and memory_set. Pick a key to edit its value, or start a new one.
          </Blank>
        ) : isNew ? (
          <NewKey
            onCreated={(key) => {
              route(dataHref(key));
              setReloads((n) => n + 1);
            }}
          />
        ) : (
          <MemoryEditor
            key={memoryKey}
            k={memoryKey}
            updatedAt={(data ?? []).find((e) => e.key === memoryKey)?.updated_at}
            onSaved={() => setReloads((n) => n + 1)}
            onDelete={remove}
          />
        )}
        <p class="muted">
          The SQL side of the store is browsable on <a href="/data/database">Data → Database</a>; the keys are browsable read-only on{' '}
          <a href="/data/memory">Data → Memory</a>.
        </p>
      </section>
    </Split>
  );
}

function MemoryEditor({ k, updatedAt, onSaved, onDelete }: { k: string; updatedAt?: string; onSaved: () => void; onDelete: (key: string) => void }) {
  const [value, setValue] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The loaded value, to tell "unsaved changes" from a saved one.
  const savedRef = useRef<string | null>(null);

  useEffect(() => {
    let alive = true;
    void (async () => {
      const { data: v, error: e, response } = await api.GET('/memory/value', { params: { query: { key: k } } });
      if (!alive) return;
      if (response.status === 404) {
        setLoadError(`No key ${k}. It may have been deleted.`);
        return;
      }
      if (!v) {
        setLoadError(errorMessage(e, 'Could not read the key.'));
        return;
      }
      savedRef.current = v.value;
      setValue(v.value);
    })();
    return () => {
      alive = false;
    };
  }, [k]);

  const save = async () => {
    if (value === null) return;
    setBusy(true);
    setError(null);
    const { error: e } = await api.PUT('/memory/value', { body: { key: k, value } });
    setBusy(false);
    if (e) setError(errorMessage(e, 'Could not write the key.'));
    else {
      savedRef.current = value;
      onSaved();
    }
  };

  const dirty = value !== null && savedRef.current !== null && value !== savedRef.current;

  if (loadError) return <p class="card error">{loadError}</p>;
  if (value === null) return <p class="card muted">Loading…</p>;
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>
            <code>{k}</code>
          </h2>
          <span class="muted">
            {bytes(new TextEncoder().encode(value).length)}
            {updatedAt ? ` · ${ago(updatedAt)}` : ''}
          </span>
          {dirty && <span class="muted">Unsaved changes</span>}
        </div>
        <div class={detail.actions}>
          <button type="button" class={`${detail.action} ${detail.danger}`} disabled={busy} onClick={() => onDelete(k)}>
            Delete
          </button>
        </div>
      </header>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      <form
        class={styles.form}
        aria-label="Memory value"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <div class={styles.field}>
          <label for="memory-value">Value</label>
          <Area id="memory-value" class="input" rows={12} max={480} value={value} onInput={(e) => setValue((e.target as HTMLTextAreaElement).value)} />
        </div>
        <div class={detail.actions}>
          <button class={`${detail.action} ${detail.primary}`} type="submit" disabled={busy}>
            {busy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </article>
  );
}

function NewKey({ onCreated }: { onCreated: (key: string) => void }) {
  const [key, setKey] = useState('');
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const create = async () => {
    setBusy(true);
    setError(null);
    const { error: e } = await api.PUT('/memory/value', { body: { key: key.trim(), value } });
    setBusy(false);
    if (e) setError(errorMessage(e, 'Could not write the key.'));
    else onCreated(key.trim());
  };

  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>New key</h2>
          <span class="muted">memory_set writes here, as it does from the agent.</span>
        </div>
      </header>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      <form
        class={styles.form}
        aria-label="New memory key"
        onSubmit={(e) => {
          e.preventDefault();
          void create();
        }}
      >
        <div class={styles.field}>
          <label for="memory-key">Key</label>
          <input id="memory-key" class="input" value={key} onInput={(e) => setKey((e.target as HTMLInputElement).value)} placeholder="self/identity" required />
        </div>
        <div class={styles.field}>
          <label for="memory-new-value">Value</label>
          <Area id="memory-new-value" class="input" rows={6} max={240} value={value} onInput={(e) => setValue((e.target as HTMLTextAreaElement).value)} />
        </div>
        <div class={detail.actions}>
          <button class={`${detail.action} ${detail.primary}`} type="submit" disabled={busy || !key.trim()}>
            {busy ? 'Creating…' : 'Create'}
          </button>
        </div>
      </form>
    </article>
  );
}
