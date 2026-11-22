import { Fragment } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
import { shortId } from '../journal';
import { recentEvents } from '../api/events';
import { headerSlot } from '../headerSlot';
import { useLive, useRecent } from '../live';
import { actionsFor, byAttention, processState, type Process } from '../processes';
import { attentionEvents, loadAttention } from '../inbox';
import { copyText } from '../clipboard';
import { ask } from '../components/Confirm';
import { Blank } from '../components/Blank';
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
  // The Run agent pop-up is shared: the list's button and the empty pane's
  // call to action both open it.
  const [picking, setPicking] = useState(false);
  return (
    <Split name="agents" noun="agents" open={!!id} foldable={false}>
      <Roster current={id} currentIsProcess={isProcess} reloadKey={renamed} picking={picking} setPicking={setPicking} />
      <section class={split.pane} aria-label="Activity">
        {id ? (
          <Activity key={`${isProcess ? 'p' : 'c'}:${id}`} id={id} isProcess={isProcess} onRenamed={() => setRenamed((n) => n + 1)} />
        ) : (
          <Blank title="Agents" action={{ label: 'Start an agent', onClick: () => setPicking(true) }}>
            Every conversation, process and goal this runtime runs, in one list. Choose one to open its activity.
          </Blank>
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
  // On a phone the chat comes first and the details sit behind a toggle below
  // it; on a desktop the panel is always beside the chat.
  const [showSide, setShowSide] = useState(false);
  return (
    <div class={styles.activity}>
      <a class={`${detail.back} ${styles.backRow}`} href="/conversations" aria-label="All agents">‹</a>
      <div class={styles.activityMain}>
        {isProcess ? <ProcessChat id={id} /> : <Chat id={id} onRenamed={onRenamed} />}
      </div>
      <button type="button" class={`buttonSecondary ${styles.sideToggle}`} aria-expanded={showSide} onClick={() => setShowSide((s) => !s)}>
        {showSide ? 'Hide details' : 'Details'}
      </button>
      <aside class={`${styles.activitySide} ${showSide ? '' : styles.sideHidden}`} aria-label="Agent details">
        {isProcess ? <ProcessPanel id={id} /> : <ConversationPanel info={conv} />}
        {orchestrator && <BoardPanel conversation={id} />}
      </aside>
    </div>
  );
}

/** The right side for a conversation agent: what identifies it. */
function ConversationPanel({
  info,
}: {
  info: { id?: string; name?: string; role?: string; status?: string; plan_mode?: string; age_seconds?: number; events_count?: number } | null | undefined;
}) {
  if (!info) return <p class="muted">Loading…</p>;
  return (
    <div class={detail.detail}>
      {info.plan_mode && info.plan_mode !== 'off' && (
        <p class={styles.planBadge} data-mode={info.plan_mode}>
          plan: {info.plan_mode}
        </p>
      )}
      {info.id && <ContextMini id={info.id} />}
      <dl class={detail.facts}>
        <dt>type</dt>
        <dd>conversation</dd>
        <dt>id</dt>
        <dd>
          <code>{info.id}</code>
          <button
            type="button"
            class={styles.copyId}
            aria-label="Copy id"
            title="Copy id"
            onClick={() => info.id && void copyText(info.id)}
          >
            <Icon name="link" size={13} />
          </button>
        </dd>
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
      <p class={styles.panelLinks}>
        {info.id && <a href={`/journal?agent=${encodeURIComponent(info.id)}`}>View in Journal</a>}
        {info.role && <a href={`/builder/roles/${encodeURIComponent(info.role)}`}>Open role</a>}
      </p>
    </div>
  );
}

/** Context use for the session, as a slim bar; refreshed when a turn ends. */
function ContextMini({ id }: { id: string }) {
  const { data } = useLive(
    async () => {
      const r = await get(api.GET('/conversations/{id}/context', { params: { path: { id } } }), 'the context');
      const ctx = (r as { context?: { used?: unknown; budget?: unknown } }).context ?? {};
      const used = typeof ctx.used === 'number' ? ctx.used : 0;
      const budget = typeof ctx.budget === 'number' ? ctx.budget : 0;
      return budget > 0 ? { used, budget } : null;
    },
    ['turn_end'],
    id,
  );
  if (!data) return null;
  const pct = Math.min(100, Math.round((data.used / data.budget) * 100));
  return (
    <div class={styles.ctxMini} title={`${data.used} of ${data.budget} tokens in context`}>
      <span class={styles.ctxMiniLabel}>context {pct}%</span>
      <span class={styles.ctxBar}>
        <span class={styles.ctxFill} style={{ width: `${pct}%` }} data-danger={pct >= 90 || undefined} />
      </span>
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
  proc?: Process;
  status?: string;
  planMode?: string;
  protected?: boolean;
  questions?: number;
  caps?: number;
};

/** The badge color for a process state or a conversation status. */
function stateClass(s: string): string {
  if (s === 'running' || s === 'active') return styles.stRunning;
  if (s === 'failing') return detail.failing;
  return styles.stMuted;
}

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

function Roster({
  current,
  currentIsProcess,
  reloadKey,
  picking,
  setPicking,
}: {
  current?: string;
  currentIsProcess: boolean;
  reloadKey: number;
  picking: boolean;
  setPicking: (v: boolean) => void;
}) {
  const { route } = useLocation();
  const [created, setCreated] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The role a new agent runs as (adr/orchestrator-board.md §2.5); default
  // orchestrator. Chosen in the pop-up the Run agent button opens.
  const [role, setRole] = useState('orchestrator');
  const picker = useRef<HTMLDialogElement>(null);
  // Row-level tools: a text filter, type chips, and per-row actions.
  const [filter, setFilter] = useState('');
  const [chip, setChip] = useState<'all' | 'conversation' | 'process' | 'goal'>('all');
  const [renaming, setRenaming] = useState<string | null>(null);
  const [renameValue, setRenameValue] = useState('');
  const renameDialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = picker.current;
    if (!d) return;
    if (picking && !d.open) d.showModal();
    else if (!picking && d.open) d.close();
  }, [picking]);
  useEffect(() => {
    const d = renameDialog.current;
    if (!d) return;
    if (renaming && !d.open) d.showModal();
    else if (!renaming && d.open) d.close();
  }, [renaming]);
  const { data: roles } = useLive(async () => {
    const r = await get(api.GET('/roles', {}), 'the role registry');
    return (r.data ?? []) as { name: string; description?: string }[];
  }, []);
  const { data, error: loadError } = useLive(
    async () => {
      const list = (await get(api.GET('/conversations', { params: { query: { limit: 200 } } }), 'conversations')).data ?? [];
      // Protected conversations pin to the top; the rest by recency.
      return [...list].sort(
        (a, b) => Number(b.protected ?? false) - Number(a.protected ?? false) || (b.updated_at ?? '').localeCompare(a.updated_at ?? ''),
      );
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
  // What waits for the operator, per agent: questions and capability requests.
  const attention = useLive(loadAttention, attentionEvents);

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

  const qByAgent = useMemo(() => {
    const m = new Map<string, number>();
    for (const q of attention.data?.questions ?? []) if (q.agent_id) m.set(q.agent_id, (m.get(q.agent_id) ?? 0) + 1);
    return m;
  }, [attention.data]);
  const capByAgent = useMemo(() => {
    const m = new Map<string, number>();
    for (const r of attention.data?.requests ?? []) if (r.agent_id) m.set(r.agent_id, (m.get(r.agent_id) ?? 0) + 1);
    return m;
  }, [attention.data]);

  const subRow = (s: Sub, parent: string, orphan = false): Row => ({
    type: 'sub-agent',
    id: s.id,
    name: shortId(s.id),
    meta: [s.task ? (s.task.length > 40 ? s.task.slice(0, 40) + '…' : s.task) : '', orphan ? `parent ${shortId(parent)}` : ''].filter(Boolean).join(' · '),
    href: `/conversations/${encodeURIComponent(s.id)}`,
    badge: s.role || 'sub-agent',
    state: s.status,
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
        proc: p,
        current: currentIsProcess && p.id === current,
      };
    }),
    ...(data ?? []).flatMap((c) => {
      const id = c.id ?? '';
      const row: Row = {
        type: 'conversation',
        id,
        name: c.name || shortId(id),
        meta: [c.role, ago(c.updated_at)].filter(Boolean).join(' · '),
        href: `/conversations/${encodeURIComponent(id)}`,
        status: c.status,
        planMode: c.plan_mode,
        protected: c.protected,
        questions: qByAgent.get(id) || undefined,
        caps: capByAgent.get(id) || undefined,
        current: !currentIsProcess && c.id === current,
      };
      const children = [...(subs.get(id)?.values() ?? [])].map((s) => subRow(s, id));
      return [row, ...children];
    }),
    // Sub-agents whose parent is not in the roster (e.g. a reaped session).
    ...[...subs.entries()].flatMap(([parent, m]) =>
      (data ?? []).some((c) => c.id === parent) || (procs.data?.list ?? []).some((p) => p.id === parent)
        ? []
        : [...m.values()].map((s) => subRow(s, parent, true)),
    ),
  ];
  const running = procs.data?.list.filter((p) => p.state !== 'stopped').length ?? 0;
  const f = filter.trim().toLowerCase();
  const textOk = (r: Row) => !f || r.name.toLowerCase().includes(f) || r.meta.toLowerCase().includes(f) || r.id.toLowerCase().includes(f);
  const chipOk = (r: Row) =>
    chip === 'all' ||
    (chip === 'conversation' && (r.type === 'conversation' || r.type === 'sub-agent')) ||
    (chip === 'process' && r.type === 'process') ||
    (chip === 'goal' && r.type === 'goal');
  const visible = rows.filter((r) => (!r.parent || !collapsed.has(r.parent)) && chipOk(r) && textOk(r));
  const hasKids = (r: Row) => (subs.get(r.id)?.size ?? 0) > 0;

  // Row actions: rename, copy link, delete a conversation; start/stop a process.
  const removeConversation = async (r: Row) => {
    if (!(await ask({ title: `Delete ${r.name}?`, message: 'The conversation and its history are removed.', confirm: 'Delete', danger: true }))) return;
    const { error: e } = await api.DELETE('/conversations/{id}', { params: { path: { id: r.id } } });
    if (e) {
      setError(errorMessage(e, 'Could not delete the conversation.'));
      return;
    }
    if (r.current) route('/conversations');
    setCreated((n) => n + 1);
  };
  const signal = async (p: Process, kind: 'start' | 'stop') => {
    const call =
      kind === 'start'
        ? api.POST('/processes/{id}/start', { params: { path: { id: p.id } } })
        : api.POST('/processes/{id}/stop', { params: { path: { id: p.id } } });
    const { error: e } = await call;
    if (e) setError(errorMessage(e, `Could not ${kind} the process.`));
  };
  const submitRename = async () => {
    if (!renaming) return;
    const name = renameValue.trim();
    if (!name) return;
    const { error: e } = await api.PATCH('/conversations/{id}', { params: { path: { id: renaming } }, body: { name } });
    if (e) {
      setError(errorMessage(e, 'Could not rename the conversation.'));
      return;
    }
    setRenaming(null);
    setCreated((n) => n + 1);
  };

  // The counts ride at the right of the shell's "Agents" label row — or, on a
  // process page (/processes/:id renders in this section), what it is.
  useEffect(() => {
    headerSlot.value = (
      <span class={styles.counts}>
        {currentIsProcess && current ? (
          <span>Process · {shortId(current)}</span>
        ) : (
          procs.data && (
            <>
              <span>{procs.data.max ? `${running} of ${procs.data.max} running` : `${running} running`}</span>
              <span>
                {(data ?? []).length} conversations · {procs.data.list.length} processes
              </span>
            </>
          )
        )}
      </span>
    );
    return () => {
      headerSlot.value = null;
    };
  });

  const chips: [typeof chip, string][] = [
    ['all', 'All'],
    ['conversation', 'Conversations'],
    ['process', 'Processes'],
    ['goal', 'Goals'],
  ];

  // Processes and goals under one label, conversations and sub-agents under
  // another; the list is ordered so each group is one run.
  const groupOf = (r: Row) => (r.type === 'process' || r.type === 'goal' ? 'Processes' : 'Conversations');

  return (
    <nav class={split.list} aria-label="Agents">
      <ListHead>
        <input class="input" type="search" aria-label="Filter agents" placeholder="Filter agents" value={filter} onInput={(e) => setFilter(e.currentTarget.value)} />
        <button type="button" class={`button ${styles.new}`} onClick={() => setPicking(true)} disabled={busy}>
          Run agent
        </button>
      </ListHead>
      <div class={styles.chips} role="group" aria-label="Filter by type">
        {chips.map(([v, label]) => (
          <button key={v} type="button" class={styles.chip} aria-pressed={chip === v} onClick={() => setChip(v)}>
            {label}
          </button>
        ))}
      </div>
      {error && <p class="error">{error}</p>}
      {loadError && <p class="error">{loadError}</p>}
      {procs.error && <p class="error">{procs.error}</p>}
      {!data && !procs.data && !loadError && !procs.error && <p class="muted">Loading…</p>}
      {data && procs.data && rows.length === 0 && (
        <p class="empty">
          No agents yet. Start one, or <a href="/builder/agents">author one in the Builder</a>.
        </p>
      )}
      {rows.length > 0 && visible.length === 0 && <p class="empty">No agent matches.</p>}
      {visible.length > 0 && (
        <ul>
          {visible.map((r, i) => (
            <Fragment key={`${r.type}:${r.id}`}>
              {groupOf(r) !== (i > 0 ? groupOf(visible[i - 1]) : null) && <li class={styles.groupHead}>{groupOf(r)}</li>}
              <li>
                <div class={styles.rowLine}>
                  <a
                    href={r.href}
                    class={styles.agentRow}
                    aria-current={r.current ? 'page' : undefined}
                    style={r.depth ? { paddingLeft: `calc(var(--space-2) + ${r.depth} * var(--space-4))` } : undefined}
                  >
                    <span class={styles.dot} data-state={r.state ?? r.status ?? ''} aria-hidden="true" />
                    <span class={styles.name}>{r.name}</span>
                    {(!!r.questions || !!r.caps) && (
                      <span class={styles.attention}>
                        {!!r.questions && (
                          <span class={`${detail.badge} ${detail.failing}`} title="Questions waiting for an answer">
                            ?{r.questions}
                          </span>
                        )}
                        {!!r.caps && (
                          <span class={`${detail.badge} ${detail.failing}`} title="Capability requests waiting">
                            cap {r.caps}
                          </span>
                        )}
                      </span>
                    )}
                    <span class={styles.meta}>
                      {r.type !== 'conversation' && <span class={detail.badge}>{r.badge ?? r.type}</span>}
                      {r.state && <span class={`${detail.badge} ${stateClass(r.state)}`}>{r.state}</span>}
                      {r.type === 'conversation' && r.status && <span class={`${detail.badge} ${stateClass(r.status)}`}>{r.status}</span>}
                      {r.planMode && (
                        <span class={detail.badge} title="Plan mode">
                          plan
                        </span>
                      )}
                      {r.protected && (
                        <span class={detail.badge} title="Protected from deletion">
                          pinned
                        </span>
                      )}
                      {r.meta}
                    </span>
                  </a>
                <span class={styles.rowActions}>
                  {r.type === 'conversation' && (
                    <>
                      <button
                        type="button"
                        aria-label={`Rename ${r.name}`}
                        title="Rename"
                        onClick={() => {
                          setRenameValue(r.name);
                          setRenaming(r.id);
                        }}
                      >
                        <Icon name="pencil" size={14} />
                      </button>
                      <button
                        type="button"
                        aria-label={`Copy link to ${r.name}`}
                        title="Copy link"
                        onClick={() => void copyText(`${window.location.origin}/conversations/${encodeURIComponent(r.id)}`)}
                      >
                        <Icon name="link" size={14} />
                      </button>
                      <button type="button" class={styles.danger} aria-label={`Delete ${r.name}`} title="Delete" onClick={() => void removeConversation(r)}>
                        <Icon name="trash" size={14} />
                      </button>
                    </>
                  )}
                  {r.type === 'sub-agent' && (
                    <button
                      type="button"
                      aria-label={`Copy link to ${r.name}`}
                      title="Copy link"
                      onClick={() => void copyText(`${window.location.origin}/conversations/${encodeURIComponent(r.id)}`)}
                    >
                      <Icon name="link" size={14} />
                    </button>
                  )}
                  {r.proc && actionsFor(r.proc).stop && (
                    <button type="button" aria-label={`Stop ${r.name}`} title="Stop" onClick={() => void signal(r.proc!, 'stop')}>
                      <Icon name="stop" size={14} />
                    </button>
                  )}
                  {r.proc && actionsFor(r.proc).start && (
                    <button type="button" aria-label={`Start ${r.name}`} title="Start" onClick={() => void signal(r.proc!, 'start')}>
                      <Icon name="play" size={14} />
                    </button>
                  )}
                </span>
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
            </Fragment>
          ))}
        </ul>
      )}
      <dialog
        ref={picker}
        class={styles.dialog}
        aria-labelledby="new-agent-title"
        onClose={() => setPicking(false)}
        onClick={(e) => {
          // A click outside the box lands on the dialog element itself.
          if (e.target === picker.current) setPicking(false);
        }}
      >
        <div class={styles.dialogBox}>
          <h2 id="new-agent-title">Run agent</h2>
          <p class="muted">Pick the role the agent runs as.</p>
          <div class={styles.roles} role="radiogroup" aria-label="Role">
            {(roles ?? [{ name: 'orchestrator' }]).map((r) => (
              <button
                key={r.name}
                type="button"
                role="radio"
                aria-checked={role === r.name}
                class={styles.roleOpt}
                onClick={() => setRole(r.name)}
              >
                <strong>{r.name}</strong>
                {r.description && <span>{r.description}</span>}
              </button>
            ))}
          </div>
          <div class={styles.dialogActions}>
            <button type="button" class="buttonSecondary" onClick={() => setPicking(false)}>
              Cancel
            </button>
            <button
              type="button"
              class="button"
              disabled={busy}
              onClick={() => {
                setPicking(false);
                void start();
              }}
            >
              Start
            </button>
          </div>
          {error && <p class="error">{error}</p>}
        </div>
      </dialog>
      <dialog
        ref={renameDialog}
        class={styles.dialog}
        aria-labelledby="rename-title"
        onClose={() => setRenaming(null)}
        onClick={(e) => {
          if (e.target === renameDialog.current) setRenaming(null);
        }}
      >
        <div class={styles.dialogBox}>
          <h2 id="rename-title">Rename agent</h2>
          <input
            class="input"
            aria-label="Name"
            value={renameValue}
            onInput={(e) => setRenameValue((e.target as HTMLInputElement).value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void submitRename();
            }}
          />
          <div class={styles.dialogActions}>
            <button type="button" class="buttonSecondary" onClick={() => setRenaming(null)}>
              Cancel
            </button>
            <button type="button" class="button" onClick={() => void submitRename()}>
              Save
            </button>
          </div>
        </div>
      </dialog>
    </nav>
  );
}
