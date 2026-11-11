import { useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
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
  const id = param(params.id as string | undefined);
  return (
    <Split name="board" noun="tasks" open={!!id}>
      <Roster current={id} />
      <section class={split.pane} aria-label="Board task">
        {id ? <TaskDetail key={id} id={id} /> : <CreateForm />}
      </section>
    </Split>
  );
}

function Roster({ current }: { current?: string }) {
  const [filter, setFilter] = useState<Status | ''>('');
  const { data, error } = useLive(async () => {
    const page = await get(api.GET('/board', { params: { query: filter ? { status: filter } : {} } }), 'the board');
    return (page.data ?? []) as Task[];
  }, boardEvents);
  return (
    <section class={split.list} aria-label="Board tasks">
      <ListHead>
        <select value={filter} onChange={(e) => setFilter((e.target as HTMLSelectElement).value as Status | '')} aria-label="Filter by status">
          <option value="">all</option>
          {statuses.map((s) => (
            <option key={s} value={s}>{s}</option>
          ))}
        </select>
        <a class={styles.new} href="/board/new" aria-label="New task"><Icon name="plus" /></a>
      </ListHead>
      {error && <p class="card error">{error}</p>}
      {!data && <p class="card muted">Loading…</p>}
      {data && data.length === 0 && <p class="empty">The board is empty.</p>}
      <ul class={styles.list}>
        {data?.map((t) => (
          <li key={t.id}>
            <a class={current === t.id ? styles.current : ''} href={boardPath(t.id)}>
              <span class={styles.title}>{t.title}</span>
              <span class={`${styles.status} ${styles[t.status]}`}>{t.status}</span>
              <span class={styles.meta}>{ago(t.created_at)}</span>
            </a>
          </li>
        ))}
      </ul>
    </section>
  );
}

function CreateForm() {
  const [title, setTitle] = useState('');
  const [spec, setSpec] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const { path } = useLocation();
  if (path === '/board/new') {
    return (
      <Area title="New task">
        <form
          onSubmit={async (e) => {
            e.preventDefault();
            if (!title.trim()) return;
            setBusy(true);
            setError('');
            const { data, error: err } = await api.POST('/board', { body: { title: title.trim(), spec: spec.trim() || undefined } });
            setBusy(false);
            if (err || !data) {
              setError(errorMessage(err, 'Could not post the task.'));
              return;
            }
            location.href = boardPath(data.id);
          }}
        >
          <label class={detail.field}>
            <span>Title</span>
            <input value={title} onInput={(e) => setTitle((e.target as HTMLInputElement).value)} required autoFocus />
          </label>
          <label class={detail.field}>
            <span>Spec — what the worker should do</span>
            <textarea value={spec} onInput={(e) => setSpec((e.target as HTMLTextAreaElement).value)} rows={6} />
          </label>
          {error && <p class="card error">{error}</p>}
          <div class={detail.actions}>
            <button class="button primary" type="submit" disabled={busy || !title.trim()}>Post to board</button>
            <a class="button" href="/board">Cancel</a>
          </div>
        </form>
      </Area>
    );
  }
  return <p class="empty">Choose a task, or post a new one.</p>;
}

function TaskDetail({ id }: { id: string }) {
  const { data, error } = useLive(async () => get(api.GET('/board/{id}', { params: { path: { id } } }), 'the task'), boardEvents);
  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState('');
  const [spec, setSpec] = useState('');
  const [status, setStatus] = useState<Status>('queued');
  const [result, setResult] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  if (error) return <p class="card error">{error}</p>;
  if (!data) return <p class="card muted">Loading…</p>;
  const t = data as unknown as Task;

  const save = async () => {
    setBusy(true);
    setErr('');
    const body: Record<string, string> = {};
    if (title.trim() && title.trim() !== t.title) body.title = title.trim();
    if (spec !== (t.spec ?? '')) body.spec = spec;
    if (status !== t.status) body.status = status;
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
    else location.href = '/board';
  };

  return (
    <Area title={t.title}>
      <div class={detail.body}>
        <p class={detail.row}><span>id</span><code>{t.id}</code></p>
        <p class={detail.row}><span>status</span><span class={`${styles.status} ${styles[t.status]}`}>{t.status}</span></p>
        <p class={detail.row}><span>posted by</span>{t.posted_by} <span class={detail.muted}>(source {t.source}, depth {t.depth})</span></p>
        <p class={detail.row}><span>claimed by</span>{t.claimed_by || '—'}</p>
        <p class={detail.row}><span>created</span>{ago(t.created_at)}</p>
        {t.finished_at && <p class={detail.row}><span>finished</span>{ago(t.finished_at)}</p>}
        {editing ? (
          <>
            <label class={detail.field}><span>Title</span><input value={title} onInput={(e) => setTitle((e.target as HTMLInputElement).value)} /></label>
            <label class={detail.field}><span>Spec</span><textarea value={spec} onInput={(e) => setSpec((e.target as HTMLTextAreaElement).value)} rows={6} /></label>
            <label class={detail.field}><span>Status</span>
              <select value={status} onChange={(e) => setStatus((e.target as HTMLSelectElement).value as Status)}>
                {statuses.map((s) => <option key={s} value={s}>{s}</option>)}
              </select>
            </label>
            <label class={detail.field}><span>Result</span><textarea value={result} onInput={(e) => setResult((e.target as HTMLTextAreaElement).value)} rows={4} /></label>
          </>
        ) : (
          <>
            {t.spec && <p class={detail.row}><span>spec</span><span class={detail.pre}>{t.spec}</span></p>}
            {t.result && <p class={detail.row}><span>result</span><span class={detail.pre}>{t.result}</span></p>}
          </>
        )}
        {err && <p class="card error">{err}</p>}
      </div>
      <div class={detail.actions}>
        {editing ? (
          <>
            <button class="button primary" onClick={save} disabled={busy}>Save</button>
            <button class="button" onClick={() => setEditing(false)}>Cancel</button>
          </>
        ) : (
          <button class="button" onClick={() => { setEditing(true); setTitle(t.title); setSpec(t.spec ?? ''); setStatus(t.status); setResult(t.result ?? ''); }}>Edit</button>
        )}
        <button class="button danger" onClick={remove}><Icon name="trash" /> Delete</button>
        <a class="button" href="/board">Back</a>
      </div>
    </Area>
  );
}
