import { useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
import { shortId } from '../journal';
import { useLive } from '../live';
import { ask } from '../components/Confirm';
import { Area } from '../components/Area';
import { ListHead, Split } from '../components/Split';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import styles from './Board.module.css';
import { Icon } from '../components/Icon';
import { param } from './Processes';

const boardEvents = ['task_*'];
const statuses = ['queued', 'running', 'done', 'failed', 'cancelled'] as const;
type Status = (typeof statuses)[number];

type Task = {
  id: string;
  title: string;
  spec?: string;
  posted_by: string;
  claimed_by?: string;
  status: Status;
  result?: string;
  source: string;
  priority?: number;
  depth: number;
  created_at: string;
  claimed_at?: string;
  finished_at?: string;
  updated_at: string;
};

/** The path of a task; its id may hold a colon or a slash. */
export function boardPath(id: string): string {
  return `/board/${encodeURIComponent(id)}`;
}

/**
 * Board (adr/orchestrator-board.md §2.6): the shared task queue beside one
 * task — create, edit, delete, live on task_* events.
 */
export function Board() {
  const { params } = useRoute();
  const { query } = useLocation();
  const id = param(params.id as string | undefined);
  // The board is one shared queue namespaced by posted_by: ?conversation=<id>
  // scopes the page to one conversation's board (adr/orchestrator-board.md §2.1).
  const conversation = typeof query.conversation === 'string' ? query.conversation : '';
  const qs = conversation ? `?conversation=${encodeURIComponent(conversation)}` : '';
  return (
    <Split name="board" noun="tasks" open={!!id}>
      <Roster current={id} conversation={conversation} qs={qs} />
      <section class={split.pane} aria-label="Board task">
        {id ? <TaskDetail key={id} id={id} qs={qs} /> : <CreateForm conversation={conversation} qs={qs} />}
      </section>
    </Split>
  );
}

/** The conversations, for the board-owner pickers. */
function useConversations() {
  return useLive(async () => {
    const list = (await get(api.GET('/conversations', { params: { query: { limit: 200 } } }), 'conversations')).data ?? [];
    return list as { id: string; name?: string }[];
  }, ['turn_start', 'turn_end']);
}

function Roster({ current, conversation, qs }: { current?: string; conversation: string; qs: string }) {
  const { route } = useLocation();
  const [filter, setFilter] = useState<Status | ''>('');
  const { data: conversations } = useConversations();
  const { data, error } = useLive(async () => {
    const page = await get(api.GET('/board', { params: { query: { limit: 1000, ...(filter ? { status: filter } : {}) } } }), 'the board');
    const all = (page.data ?? []) as Task[];
    return conversation ? all.filter((t) => t.posted_by === conversation) : all;
  }, boardEvents, `${conversation}:${filter}`);
  return (
    <section class={split.list} aria-label="Board tasks">
      <ListHead>
        <select
          value={conversation}
          onChange={(e) => {
            const v = (e.target as HTMLSelectElement).value;
            route(v ? `/board?conversation=${encodeURIComponent(v)}` : '/board');
          }}
          aria-label="Conversation — whose board to show"
          title="One conversation's board: the tasks it posted"
        >
          <option value="">all conversations</option>
          {(conversations ?? []).map((c) => (
            <option key={c.id} value={c.id}>{c.name || shortId(c.id)}</option>
          ))}
        </select>
        <select value={filter} onChange={(e) => setFilter((e.target as HTMLSelectElement).value as Status | '')} aria-label="Filter by status">
          <option value="">all</option>
          {statuses.map((s) => (
            <option key={s} value={s}>{s}</option>
          ))}
        </select>
        <a class={styles.new} href={`/board/new${qs}`} aria-label="New task"><Icon name="plus" /></a>
      </ListHead>
      {error && <p class="card error">{error}</p>}
      {!data && <p class="card muted">Loading…</p>}
      {data && data.length === 0 && <p class="empty">{conversation ? 'Nothing on this conversation’s board.' : 'The board is empty.'}</p>}
      <ul class={styles.list}>
        {data?.map((t) => (
          <li key={t.id}>
            <a class={current === t.id ? styles.current : ''} href={boardPath(t.id) + qs}>
              <span class={styles.title}>{t.title}</span>
              <span class={`${styles.status} ${styles[t.status]}`}>{t.status}</span>
              <span class={styles.meta}>{shortId(t.posted_by)} · {ago(t.created_at)}</span>
            </a>
          </li>
        ))}
      </ul>
    </section>
  );
}

function CreateForm({ conversation, qs }: { conversation: string; qs: string }) {
  const [title, setTitle] = useState('');
  const [spec, setSpec] = useState('');
  const [priority, setPriority] = useState(0);
  // Whose board the task lands on: the conversation the page is scoped to, or
  // the operator's own.
  const [poster, setPoster] = useState(conversation || 'operator');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const { path, route } = useLocation();
  const { data: conversations } = useConversations();
  if (path !== '/board/new') return <p class="empty">Choose a task, or post a new one.</p>;
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href={`/board${qs}`} aria-label="All tasks">
          ‹
        </a>
        <div class={detail.title}>
          <h2>New task</h2>
        </div>
      </header>
      <form
        class={styles.form}
        aria-label="New task"
        onSubmit={async (e) => {
          e.preventDefault();
          if (!title.trim()) return;
          setBusy(true);
          setError('');
          const { data, error: err } = await api.POST('/board', {
            body: { title: title.trim(), spec: spec.trim() || undefined, priority, posted_by: poster === 'operator' ? undefined : poster },
          });
          setBusy(false);
          if (err || !data) {
            setError(errorMessage(err, 'Could not post the task.'));
            return;
          }
          route(boardPath(data.id) + qs);
        }}
      >
        <div class={styles.field}>
          <label for="new-task-poster">Board of</label>
          <select id="new-task-poster" class="input" value={poster} onChange={(e) => setPoster((e.target as HTMLSelectElement).value)}>
            <option value="operator">operator</option>
            {(conversations ?? []).map((c) => (
              <option key={c.id} value={c.id}>{c.name || shortId(c.id)}</option>
            ))}
          </select>
        </div>
        <div class={styles.field}>
          <label for="new-task-title">Title</label>
          <input id="new-task-title" class="input" value={title} onInput={(e) => setTitle((e.target as HTMLInputElement).value)} required autoFocus />
        </div>
        <div class={styles.field}>
          <label for="new-task-spec">Spec — what the worker should do</label>
          <Area id="new-task-spec" class="input" rows={6} max={400} value={spec} onInput={(e) => setSpec((e.target as HTMLTextAreaElement).value)} />
        </div>
        <div class={styles.field}>
          <label for="new-task-priority">Priority (0–10, higher claimed first)</label>
          <input id="new-task-priority" class="input" type="number" min={0} max={10} value={priority} onInput={(e) => setPriority(Number((e.target as HTMLInputElement).value))} />
        </div>
        {error && <p class="error" role="alert">{error}</p>}
        <div class={detail.actions}>
          <button class={`${detail.action} ${detail.primary}`} type="submit" disabled={busy || !title.trim()}>Post to board</button>
          <a class={`${detail.action} ${detail.secondary}`} href={`/board${qs}`}>Cancel</a>
        </div>
      </form>
    </article>
  );
}

function TaskDetail({ id, qs }: { id: string; qs: string }) {
  const { route } = useLocation();
  const { data, error } = useLive(async () => get(api.GET('/board/{id}', { params: { path: { id } } }), 'the task'), boardEvents);
  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState('');
  const [spec, setSpec] = useState('');
  const [status, setStatus] = useState<Status>('queued');
  const [priority, setPriority] = useState(0);
  const [result, setResult] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  if (error) return <p class="card error">{error}</p>;
  if (!data) return <p class="card muted">Loading…</p>;
  const t = data as unknown as Task;

  const save = async () => {
    setBusy(true);
    setErr('');
    const body: Record<string, string | number> = {};
    if (title.trim() && title.trim() !== t.title) body.title = title.trim();
    if (spec !== (t.spec ?? '')) body.spec = spec;
    if (status !== t.status) body.status = status;
    if (priority !== (t.priority ?? 0)) body.priority = priority;
    if (result !== (t.result ?? '')) body.result = result;
    const { error: e } = await api.PATCH('/board/{id}', { params: { path: { id } }, body });
    setBusy(false);
    if (e) setErr(errorMessage(e, 'Could not save.'));
    else setEditing(false);
  };

  const remove = async () => {
    if (!(await ask({ title: 'Delete task', message: `Delete ${t.title}? This removes the task row for good.`, confirm: 'Delete' }))) return;
    const { error: e } = await api.DELETE('/board/{id}', { params: { path: { id } } });
    if (e) setErr(errorMessage(e, 'Could not delete.'));
    else route(`/board${qs}`);
  };

  const startEdit = () => {
    setEditing(true);
    setTitle(t.title);
    setSpec(t.spec ?? '');
    setStatus(t.status);
    setPriority(t.priority ?? 0);
    setResult(t.result ?? '');
  };

  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href={`/board${qs}`} aria-label="All tasks">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{t.title}</h2>
          <span class="muted">
            <span class={`${detail.badge} ${styles[t.status]}`}>{t.status}</span> <code>{shortId(t.id)}</code>
          </span>
        </div>
        <div class={detail.actions}>
          {editing ? (
            <>
              <button class={`${detail.action} ${detail.primary}`} onClick={() => void save()} disabled={busy}>Save</button>
              <button class={`${detail.action} ${detail.secondary}`} onClick={() => setEditing(false)}>Cancel</button>
            </>
          ) : (
            <button class={`${detail.action} ${detail.primary}`} onClick={startEdit}>Edit</button>
          )}
          <button class={`${detail.action} ${detail.danger}`} onClick={() => void remove()}>
            <Icon name="trash" /> Delete
          </button>
        </div>
      </header>
      {editing ? (
        <form
          class={styles.form}
          aria-label={`Edit ${t.title}`}
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
        >
          <div class={styles.field}>
            <label for="edit-title">Title</label>
            <input id="edit-title" class="input" value={title} onInput={(e) => setTitle((e.target as HTMLInputElement).value)} />
          </div>
          <div class={styles.field}>
            <label for="edit-spec">Spec</label>
            <Area id="edit-spec" class="input" rows={6} max={400} value={spec} onInput={(e) => setSpec((e.target as HTMLTextAreaElement).value)} />
          </div>
          <div class={styles.field}>
            <label for="edit-priority">Priority</label>
            <input id="edit-priority" class="input" type="number" min={0} max={10} value={priority} onInput={(e) => setPriority(Number((e.target as HTMLInputElement).value))} />
          </div>
          <div class={styles.field}>
            <label for="edit-status">Status</label>
            <select id="edit-status" class="input" value={status} onChange={(e) => setStatus((e.target as HTMLSelectElement).value as Status)}>
              {statuses.map((s) => <option key={s} value={s}>{s}</option>)}
            </select>
          </div>
          <div class={styles.field}>
            <label for="edit-result">Result</label>
            <Area id="edit-result" class="input" rows={4} max={400} value={result} onInput={(e) => setResult((e.target as HTMLTextAreaElement).value)} />
          </div>
          {err && <p class="error" role="alert">{err}</p>}
        </form>
      ) : (
        <>
          <dl class={detail.facts}>
            <dt>id</dt>
            <dd><code>{t.id}</code></dd>
            <dt>posted by</dt>
            <dd>
              {t.posted_by} <span class="muted">(source {t.source}, depth {t.depth})</span>
            </dd>
            <dt>claimed by</dt>
            <dd>{t.claimed_by || '—'}</dd>
            <dt>created</dt>
            <dd>{ago(t.created_at)}</dd>
            {t.finished_at && (
              <>
                <dt>finished</dt>
                <dd>{ago(t.finished_at)}</dd>
              </>
            )}
          </dl>
          {t.spec && (
            <section class={detail.section}>
              <h3>Spec</h3>
              <p class={styles.pre}>{t.spec}</p>
            </section>
          )}
          {t.result && (
            <section class={detail.section}>
              <h3>Result</h3>
              <p class={styles.pre}>{t.result}</p>
            </section>
          )}
          {err && <p class="error" role="alert">{err}</p>}
        </>
      )}
    </article>
  );
}
