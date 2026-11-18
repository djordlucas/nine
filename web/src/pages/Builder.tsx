import type { ComponentChildren } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get, type Schemas } from '../api/client';
import { useLive } from '../live';
import { bytes } from '../system';
import { ask } from '../components/Confirm';
import { Area } from '../components/Area';
import { Toggle } from '../components/Toggle';
import { Icon } from '../components/Icon';
import { ListHead, Split } from '../components/Split';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import { param } from './Processes';
import { PluginsView, ToolsView } from './Tools';
import tools from './Tools.module.css';
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
    <div class={tools.section}>
      <nav class={tools.views} aria-label="Builder views">
        {views.map((v) => (
          <a key={v.path} href={v.path} class={tools.view} aria-current={v.path === view ? 'page' : undefined}>
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

/** What a Builder view's pane shows when nothing is selected: a word on what
 * the section is for, so the page is never a bare pane. */
function Blank({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>{title}</h2>
        </div>
      </header>
      <p class="muted">{children}</p>
    </article>
  );
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

  return (
    <Split name="builder-agents" noun="agents" open={!!id}>
      <nav class={split.list} aria-label="Agents">
        <ListHead>
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
        </ListHead>
        {outcome && (
          <p class="muted" role="status">
            {outcome}
          </p>
        )}
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && data.agents.length === 0 && <p class="empty">No agents yet. Create one, or import a bundle.</p>}
        {data && data.agents.length > 0 && (
          <ul>
            {data.agents.map((a) => (
              <li key={a.id}>
                <a href={agentHref(a.id)} class={split.row} aria-current={a.id === id ? 'page' : undefined}>
                  <span className={`${detail.badge} ${a.status === 'staged' ? detail.failing : ''}`}>{a.status}</span>
                  <span class={split.name}>{a.definition.name}</span>
                  <span class={split.meta}>
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
      setAgent(a);
      setName(a.definition.name);
      setRole(a.definition.role ?? '');
      setSkillsText((a.definition.skills ?? []).join(', '));
      setSelfModel(a.definition.self_model ? JSON.stringify(a.definition.self_model, null, 2) : '');
      setReflection(a.definition.reflection ? JSON.stringify(a.definition.reflection, null, 2) : '');
    })();
    return () => {
      alive = false;
    };
  }, [id, isNew, reload]);

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
        </div>
        {!isNew && agent && (
          <div class={detail.actions}>
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
        <div class={styles.field}>
          <label for="agent-skills">Skills (comma-separated)</label>
          <input
            id="agent-skills"
            class="input"
            value={skillsText}
            onInput={(e) => setSkillsText((e.target as HTMLInputElement).value)}
            placeholder={skills.map((s) => s.name).join(', ')}
          />
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
            {isNew ? 'Create' : 'Save'}
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
  const { data, error } = useLive(async () => {
    const roles = (await get(api.GET('/roles'), 'roles')).data ?? [];
    return [...roles].sort((a, b) => a.name.localeCompare(b.name));
  }, [], reloads);

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
          <a class="button" href={roleHref('new')}>
            New role
          </a>
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && data.length === 0 && <p class="empty">No roles.</p>}
        {data && data.length > 0 && (
          <ul>
            {data.map((r) => (
              <li key={r.name}>
                <a href={roleHref(r.name)} class={split.row} aria-current={r.name === name ? 'page' : undefined}>
                  <span class={detail.badge}>{r.all_tools ? 'all tools' : `${r.tools?.length ?? 0} tools`}</span>
                  <span class={split.name}>{r.name}</span>
                  <span class={split.meta}>
                    {[r.leaf ? 'leaf' : null, r.delegates ? 'delegates' : null].filter(Boolean).join(' · ')}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Role">
        {data && (name ? <RoleForm key={name} name={name} roles={data} onSaved={() => setReloads((n) => n + 1)} onDelete={remove} /> : (
          <Blank title="Roles">
            A role is a persona with a tool allowlist, stored as a skill with a role block. Pick one to edit it, or start from New role.
          </Blank>
        ))}
      </section>
    </Split>
  );
}

function RoleForm({ name, roles, onSaved, onDelete }: { name: string; roles: RoleInfo[]; onSaved: () => void; onDelete: (name: string) => void }) {
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

  const save = async () => {
    setBusy(true);
    setError(null);
    const tools = toolsText
      .split(',')
      .map((t) => t.trim())
      .filter(Boolean);
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
              <input id="role-tools" class="input" value={toolsText} onInput={(e) => setToolsText((e.target as HTMLInputElement).value)} placeholder="shell, file_read, file_write" />
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
            {isNew ? 'Create' : 'Save'}
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
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && shown.length === 0 && <p class="empty">No skill matches.</p>}
        {shown.length > 0 && (
          <ul>
            {shown.map((s) => (
              <li key={s.name}>
                <a href={skillHref(s.name)} class={split.row} aria-current={s.name === name ? 'page' : undefined}>
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
      setSource(s.source);
      setSkillName(s.name);
      setDescription(s.description ?? '');
      setTagsText((s.tags ?? []).join(', '));
      setContent(s.content);
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
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (loadError) return <p class="card error">{loadError}</p>;
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>{isNew ? 'New skill' : name}</h2>
          {source && <span class={detail.badge}>{source}</span>}
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
              {isNew ? 'Create' : 'Save'}
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
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && shown.length === 0 && <p class="empty">{data.length ? 'No key matches.' : 'Nothing in memory yet.'}</p>}
        {shown.length > 0 && (
          <ul>
            {shown.map((e: MemoryEntry) => (
              <li key={e.key}>
                <a href={dataHref(e.key)} class={split.row} aria-current={e.key === memoryKey ? 'page' : undefined}>
                  <span class={`${split.name} ${tools.wide}`}>{e.key}</span>
                  <span class={split.meta}>{bytes(e.bytes)}</span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Memory key">
        {memoryKey !== undefined && !isNew ? (
          <MemoryEditor key={memoryKey} k={memoryKey} onSaved={() => setReloads((n) => n + 1)} onDelete={remove} />
        ) : (
          <NewKey
            onCreated={(key) => {
              route(dataHref(key));
              setReloads((n) => n + 1);
            }}
          />
        )}
        <p class="muted">
          The SQL side of the store is browsable on <a href="/data/database">Data → Database</a>.
        </p>
      </section>
    </Split>
  );
}

function MemoryEditor({ k, onSaved, onDelete }: { k: string; onSaved: () => void; onDelete: (key: string) => void }) {
  const [value, setValue] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

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
    else onSaved();
  };

  if (loadError) return <p class="card error">{loadError}</p>;
  if (value === null) return <p class="card muted">Loading…</p>;
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>
            <code>{k}</code>
          </h2>
          <span class="muted">{bytes(new TextEncoder().encode(value).length)}</span>
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
            Save
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
            Create
          </button>
        </div>
      </form>
    </article>
  );
}
