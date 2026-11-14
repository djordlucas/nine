import { useMemo, useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
import { shortId } from '../journal';
import { recentEvents } from '../api/events';
import { useLive, useRecent } from '../live';
import { byAttention, processState } from '../processes';
import { Chat } from './Chat';
import { ListHead, Split } from '../components/Split';
import { Icon } from '../components/Icon';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import styles from './Agents.module.css';
import { param, processEvents, ProcessChat, ProcessPanel, processPath } from './Processes';
import { BoardPanel } from './Board';

const COLLAPSED_KEY = 'nine.agents.collapsed';

function readCollapsed(): Set<string> {
  try {
    return new Set((localStorage.getItem(COLLAPSED_KEY) ?? '').split(',').filter(Boolean));
  } catch {
    return new Set();
  }
}

function writeCollapsed(s: Set<string>) {
  try {
    if (s.size) localStorage.setItem(COLLAPSED_KEY, [...s].join(','));
    else localStorage.removeItem(COLLAPSED_KEY);
  } catch {
    // Not remembered; the list still folds.
  }
}

/**
 * Agents (adr/web-ui.md §5): every agent the runtime runs — conversations,
 * processes, goals — in one list beside its activity. Every agent opens the
 * same activity shape (the chat); what is specific to a goal or process sits
 * in the panel on the right. On a desktop the list sits beside the pane, on a
 * phone it gives way to it. /processes/… deep links open the process here.
 */
export function Agents() {
  const { params } = useRoute();
  const { path } = useLocation();
  const id = param(params.id as string | undefined);
  const isProcess = path.startsWith('/processes');
  // Bumped by a rename, so the list reads the new name.
  const [renamed, setRenamed] = useState(0);
  return (
    <Split name="agents" noun="agents" open={!!id}>
      <Roster current={id} currentIsProcess={isProcess} reloadKey={renamed} />
      <section class={split.pane} aria-label="Activity">
        {id ? (
          <Activity key={`${isProcess ? 'p' : 'c'}:${id}`} id={id} isProcess={isProcess} onRenamed={() => setRenamed((n) => n + 1)} />
        ) : (
          <p class="empty">Choose an agent, or start a new one.</p>
        )}
      </section>
    </Split>
  );
}

/** The activity pane: the agent's chat in the middle, its specifics on the right. */
function Activity({ id, isProcess, onRenamed }: { id: string; isProcess: boolean; onRenamed: () => void }) {
  // The list carries the roster's fields; the single-agent GET does not.
  const { data: conv } = useLive(
    async () => {
      if (isProcess) return undefined;
      const list = (await get(api.GET('/conversations', { params: { query: { limit: 200 } } }), 'agents')).data ?? [];
      return list.find((c) => c.id === id);
    },
    ['turn_start', 'turn_end'],
    `${id}:${isProcess}`,
  );
  // The board is local to the session; it rides on the right of an
  // orchestrator agent (the role that posts to it).
  const orchestrator = !isProcess && (!conv?.role || conv.role === 'orchestrator');
  return (
    <div class={styles.activity}>
      <div class={styles.activityMain}>
        {isProcess ? <ProcessChat id={id} /> : <Chat id={id} onRenamed={onRenamed} />}
      </div>
      <aside class={styles.activitySide} aria-label="Agent details">
        <a class={detail.back} href="/conversations" aria-label="All agents">‹</a>
        {isProcess ? <ProcessPanel id={id} /> : <ConversationPanel info={conv} />}
        {orchestrator && <BoardPanel conversation={id} />}
      </aside>
    </div>
  );
}

/** The right side for a conversation agent: what identifies it. */
function ConversationPanel({ info }: { info: { id?: string; name?: string; role?: string; status?: string; age_seconds?: number; events_count?: number } | null | undefined }) {
  if (!info) return <p class="muted">Loading…</p>;
  return (
    <div class={detail.detail}>
      <dl class={detail.facts}>
        <dt>type</dt>
        <dd>conversation</dd>
        <dt>id</dt>
        <dd><code>{info.id}</code></dd>
        {info.name && (
          <>
            <dt>name</dt>
            <dd>{info.name}</dd>
          </>
        )}
        {info.role && (
          <>
            <dt>role</dt>
            <dd>{info.role}</dd>
          </>
        )}
        <dt>status</dt>
        <dd>{info.status}</dd>
        <dt>last activity</dt>
        <dd>{info.age_seconds !== undefined ? ago(new Date(Date.now() - info.age_seconds * 1000).toISOString()) : ''}</dd>
        <dt>events</dt>
        <dd>{info.events_count ?? 0}</dd>
      </dl>
    </div>
  );
}

type Row = {
  type: 'conversation' | 'process' | 'goal' | 'sub-agent';
  id: string;
  name: string;
  meta: string;
  href: string;
  state?: string;
  badge?: string;
  depth?: number;
  parent?: string;
  current: boolean;
};

type Sub = { id: string; role?: string; task?: string; status?: string };

/**
 * Sub-agents by parent session. The durable link is the board: a delegated
 * run records posted_by (the parent) and claimed_by (the sub-agent); the
 * journal's sub_agent_* events add the role and the live status.
 */
function useSubAgents(board: { claimed_by?: string; posted_by?: string; source?: string; title?: string; status?: string }[] | null): Map<string, Map<string, Sub>> {
  const events = useRecent(['sub_agent_start', 'sub_agent_end'], 400, recentEvents);
  return useMemo(() => {
    const byParent = new Map<string, Map<string, Sub>>();
    const str = (v: unknown) => (typeof v === 'string' ? v : '');
    const put = (parent: string, s: Sub) => {
      let m = byParent.get(parent);
      if (!m) byParent.set(parent, (m = new Map()));
      const cur = m.get(s.id);
      m.set(s.id, { ...cur, ...s, role: s.role || cur?.role, task: s.task || cur?.task, status: s.status || cur?.status });
    };
    for (const t of board ?? []) {
      if (!t.claimed_by || !t.posted_by || t.source === 'post') continue;
      put(t.posted_by, { id: t.claimed_by, task: t.title, status: t.status });
    }
    for (const e of events) {
      const p = (e.payload ?? {}) as Record<string, unknown>;
      const subId = str(p.sub_id);
      if (!subId || !e.agent_id) continue;
      put(e.agent_id, { id: subId, role: str(p.role) || undefined, task: str(p.task) || undefined, status: str(p.status) || undefined });
    }
    return byParent;
  }, [board, events]);
}

function Roster({ current, currentIsProcess, reloadKey }: { current?: string; currentIsProcess: boolean; reloadKey: number }) {
  const { route } = useLocation();
  const [created, setCreated] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The role a new agent runs as (adr/orchestrator-board.md §2.5); default
  // orchestrator.
  const [role, setRole] = useState('orchestrator');
  const { data: roles } = useLive(async () => {
    const r = await get(api.GET('/roles', {}), 'the role registry');
    return (r.data ?? []) as { name: string; description?: string }[];
  }, []);
  const { data, error: loadError } = useLive(
    async () => {
      const list = (await get(api.GET('/conversations', { params: { query: { limit: 200 } } }), 'conversations')).data ?? [];
      return [...list].sort((a, b) => (b.updated_at ?? '').localeCompare(a.updated_at ?? ''));
    },
    ['turn_start', 'turn_end', 'session_created'],
    `${created}:${reloadKey}`,
  );
  const procs = useLive(
    async () => {
      const [list, status] = await Promise.all([
        get(api.GET('/processes', { params: { query: { limit: 1000 } } }), 'processes'),
        get(api.GET('/status'), 'the daemon status'),
      ]);
      return { list: byAttention(list.data ?? []), max: status.max_running };
    },
    processEvents,
  );
  // A goal is its pursue process: the goal attaches to that process's row.
  const goals = useLive(
    async () => (await get(api.GET('/goals', { params: { query: { limit: 1000 } } }), 'goals')).data ?? [],
    ['goal_*', ...processEvents],
  );
  // The board carries the durable parent → sub-agent links.
  const board = useLive(
    async () => (await get(api.GET('/board', { params: { query: { limit: 1000 } } }), 'the board')).data ?? [],
    ['task_*'],
  );

  const start = async () => {
    setBusy(true);
    setError(null);
    // Interactive: the agent may ask the operator questions.
    const { data: made, error: e } = await api.POST('/conversations', { body: { interactive: true, role } });
    setBusy(false);
    if (!made) {
      setError(errorMessage(e, 'Could not start an agent.'));
      return;
    }
    setCreated((n) => n + 1);
    route(`/conversations/${made.id}`);
  };

  const subs = useSubAgents(board.data);
  // Parents whose sub-agent children are folded away, remembered per browser.
  const [collapsed, setCollapsed] = useState<Set<string>>(() => readCollapsed());
  const toggle = (id: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      writeCollapsed(next);
      return next;
    });
  const subRow = (s: Sub, parent: string): Row => ({
    type: 'sub-agent',
    id: s.id,
    name: shortId(s.id),
    meta: [s.status, s.task ? (s.task.length > 40 ? s.task.slice(0, 40) + '…' : s.task) : ''].filter(Boolean).join(' · '),
    href: `/conversations/${encodeURIComponent(s.id)}`,
    badge: s.role || 'sub-agent',
    depth: 1,
    parent,
    current: !currentIsProcess && s.id === current,
  });
  const rows: Row[] = [
    ...(procs.data?.list ?? []).map((p) => {
      const goal = p.goal ? goals.data?.find((g) => g.id === p.goal) : undefined;
      return {
        type: (goal ? 'goal' : 'process') as Row['type'],
        id: p.id,
        name: goal?.description || p.id,
        meta: [goal?.status, p.tool, p.trigger !== '—' ? p.trigger : ''].filter(Boolean).join(' · '),
        href: processPath(p.id),
        state: processState(p),
        current: currentIsProcess && p.id === current,
      };
    }),
    ...(data ?? []).flatMap((c) => {
      const row: Row = {
        type: 'conversation',
        id: c.id ?? '',
        name: c.name || shortId(c.id ?? ''),
        meta: [c.role, c.status, ago(c.updated_at)].filter(Boolean).join(' · '),
        href: `/conversations/${encodeURIComponent(c.id ?? '')}`,
        current: !currentIsProcess && c.id === current,
      };
      const children = [...(subs.get(c.id ?? '')?.values() ?? [])].map((s) => subRow(s, c.id ?? ''));
      return [row, ...children];
    }),
    // Sub-agents whose parent is not in the roster (e.g. a reaped session).
    ...[...subs.entries()].flatMap(([parent, m]) =>
      (data ?? []).some((c) => c.id === parent) || (procs.data?.list ?? []).some((p) => p.id === parent)
        ? []
        : [...m.values()].map((s) => subRow(s, parent)),
    ),
  ];
  const running = procs.data?.list.filter((p) => p.state !== 'stopped').length ?? 0;
  const visible = rows.filter((r) => !r.parent || !collapsed.has(r.parent));
  const hasKids = (r: Row) => (subs.get(r.id)?.size ?? 0) > 0;

  return (
    <nav class={split.list} aria-label="Agents">
      <ListHead>
        <p class={split.heading}>
          {procs.data && (
            <>
              <span>{procs.data.max ? `${running} of ${procs.data.max} running` : `${running} running`}</span>
              <span>{rows.length} agents</span>
            </>
          )}
        </p>
        <select
          value={role}
          onChange={(e) => setRole((e.target as HTMLSelectElement).value)}
          aria-label="Role for the new agent"
          title="The role the new agent runs as — default orchestrator"
        >
          {(roles ?? [{ name: 'orchestrator' }]).map((r) => (
            <option key={r.name} value={r.name}>{r.name}</option>
          ))}
        </select>
        <button type="button" class={`button ${styles.new}`} onClick={() => void start()} disabled={busy}>
          New agent
        </button>
      </ListHead>
      {error && <p class="error">{error}</p>}
      {loadError && <p class="error">{loadError}</p>}
      {procs.error && <p class="error">{procs.error}</p>}
      {!data && !procs.data && !loadError && !procs.error && <p class="muted">Loading…</p>}
      {data && procs.data && rows.length === 0 && <p class="empty">No agents yet.</p>}
      {rows.length > 0 && (
        <ul>
          {visible.map((r) => (
            <li key={`${r.type}:${r.id}`}>
              <div class={styles.rowLine}>
                <a
                  href={r.href}
                  class={styles.agentRow}
                  aria-current={r.current ? 'page' : undefined}
                  style={r.depth ? { paddingLeft: `calc(var(--space-2) + ${r.depth} * var(--space-4))` } : undefined}
                >
                  <span class={styles.name}>{r.name}</span>
                  <span class={styles.meta}>
                    {r.type !== 'conversation' && <span class={detail.badge}>{r.badge ?? r.type}</span>}
                    {r.state && <span class={`${detail.badge} ${detail[r.state]}`}>{r.state}</span>}
                    {r.meta}
                  </span>
                </a>
                {hasKids(r) && (
                  <button
                    type="button"
                    class={styles.toggle}
                    aria-expanded={!collapsed.has(r.id)}
                    aria-label={`${collapsed.has(r.id) ? 'Show' : 'Hide'} the sub-agents of ${r.name}`}
                    title={collapsed.has(r.id) ? 'Show sub-agents' : 'Hide sub-agents'}
                    onClick={() => toggle(r.id)}
                  >
                    <Icon name="next" size={14} />
                  </button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </nav>
  );
}
