import { useState } from 'preact/hooks';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
import { shortId } from '../journal';
import { useLive } from '../live';
import { ask } from '../components/Confirm';
import { Area } from '../components/Area';
import detail from '../components/Detail.module.css';
import styles from './Board.module.css';
import { Icon } from '../components/Icon';

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
  role?: string;
  priority?: number;
  depth: number;
  created_at: string;
  claimed_at?: string;
  finished_at?: string;
  updated_at: string;
};

/**
 * One session's board, in the right panel of an orchestrator agent's activity
 * (adr/orchestrator-board.md §2.6): the session's tasks — the board is local
 * to the session (posted_by) — each expandable, plus posting a new one.
 */
export function BoardPanel({ conversation }: { conversation: string }) {
  const [title, setTitle] = useState('');
  const [spec, setSpec] = useState('');
  const [role, setRole] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  // The posting form stays folded until asked for: on the chat page it would
  // compete with the message box for the operator's attention.
  const [posting, setPosting] = useState(false);
  // The one task being edited, if any.
  const [editing, setEditing] = useState<string | null>(null);
  const [eTitle, setETitle] = useState('');
  const [eSpec, setESpec] = useState('');
  const [ePriority, setEPriority] = useState(0);
  const [eRole, setERole] = useState('');
  const [eStatus, setEStatus] = useState<Status>('queued');
  const [eResult, setEResult] = useState('');
  // Leaf roles a posted task may name (adr/agents-concept.md A6, Q15/Q16).
  const { data: leafRoles } = useLive(async () => {
    const r = await get(api.GET('/roles', {}), 'the role registry');
    return ((r.data ?? []) as { name: string; leaf?: boolean }[]).filter((x) => x.leaf).map((x) => x.name);
  }, []);
  const { data, error: loadError } = useLive(async () => {
    const page = await get(api.GET('/board', { params: { query: { limit: 1000 } } }), 'the board');
    return ((page.data ?? []) as Task[]).filter((t) => t.posted_by === conversation);
  }, boardEvents, conversation);

  const post = async (e: Event) => {
    e.preventDefault();
    if (!title.trim()) return;
    setBusy(true);
    setError('');
    const { data: made, error: err } = await api.POST('/board', {
      body: { title: title.trim(), spec: spec.trim() || undefined, role: role || undefined, priority: 0, posted_by: conversation },
    });
    setBusy(false);
    if (err || !made) {
      setError(errorMessage(err, 'Could not post the task.'));
      return;
    }
    setTitle('');
    setSpec('');
    setRole('');
  };

  const remove = async (t: Task) => {
    if (!(await ask({ title: 'Delete task', message: `Delete ${t.title}? This removes the task row for good.`, confirm: 'Delete' }))) return;
    const { error: e } = await api.DELETE('/board/{id}', { params: { path: { id: t.id } } });
    if (e) setError(errorMessage(e, 'Could not delete.'));
  };

  const startEdit = (t: Task) => {
    setEditing(t.id);
    setETitle(t.title);
    setESpec(t.spec ?? '');
    setEPriority(t.priority ?? 0);
    setERole(t.role ?? '');
    setEStatus(t.status);
    setEResult(t.result ?? '');
  };

  const save = async (t: Task) => {
    setBusy(true);
    setError('');
    const body: Record<string, string | number> = {};
    if (eTitle.trim() && eTitle.trim() !== t.title) body.title = eTitle.trim();
    if (eSpec !== (t.spec ?? '')) body.spec = eSpec;
    if (eStatus !== t.status) body.status = eStatus;
    if (ePriority !== (t.priority ?? 0)) body.priority = ePriority;
    if (eRole !== (t.role ?? '')) body.role = eRole;
    if (eResult !== (t.result ?? '')) body.result = eResult;
    const { error: e } = await api.PATCH('/board/{id}', { params: { path: { id: t.id } }, body });
    setBusy(false);
    if (e) setError(errorMessage(e, 'Could not save.'));
    else setEditing(null);
  };

  return (
    <section class={styles.panel} aria-label="Board">
      <h3 class={styles.panelTitle}>Board{data?.length ? ` · ${data.length}` : ''}</h3>
      {loadError && <p class="error">{loadError}</p>}
      {!data && !loadError && <p class="muted">Loading…</p>}
      {error && <p class="error" role="alert">{error}</p>}
      {data && data.length === 0 && <p class="empty">No tasks on this agent’s board.</p>}
      {!posting ? (
        <button type="button" class={`button ${styles.newTask}`} onClick={() => setPosting(true)}>
          New task
        </button>
      ) : (
        <form class={styles.form} aria-label="Post a task to this board" onSubmit={(e) => void post(e)}>
          <div class={styles.field}>
            <label for={`board-title-${conversation}`}>Title</label>
            <input id={`board-title-${conversation}`} class="input" value={title} onInput={(e) => setTitle((e.target as HTMLInputElement).value)} placeholder="Title" required />
          </div>
          <div class={styles.field}>
            <label for={`board-spec-${conversation}`}>Spec — what the worker should do</label>
            <Area id={`board-spec-${conversation}`} class="input" rows={2} max={120} value={spec} onInput={(e) => setSpec((e.target as HTMLTextAreaElement).value)} />
          </div>
          <div class={styles.field}>
            <label for={`board-role-${conversation}`}>Worker role</label>
            <select id={`board-role-${conversation}`} class="input" value={role} onChange={(e) => setRole((e.target as HTMLSelectElement).value)}>
              <option value="">worker's own role</option>
              {(leafRoles ?? []).map((r) => <option key={r} value={r}>{r}</option>)}
            </select>
          </div>
          <div class={detail.actions}>
            <button class={`${detail.action} ${detail.primary}`} type="submit" disabled={busy || !title.trim()}>
              Post
            </button>
            <button type="button" class={detail.action} onClick={() => setPosting(false)}>
              Cancel
            </button>
          </div>
        </form>
      )}

      {data && data.length > 0 && (
        <ul class={styles.list}>
          {data.map((t) => (
            <li key={t.id}>
              <details class={styles.task}>
                <summary>
                  <span class={styles.title}>{t.title}</span>
                  <span class={`${styles.status} ${styles[t.status]}`}>{t.status}</span>
                </summary>
                <div class={styles.taskBody}>
                  {editing === t.id ? (
                    <>
                      <div class={styles.field}>
                        <label>Title</label>
                        <input class="input" value={eTitle} onInput={(e) => setETitle((e.target as HTMLInputElement).value)} />
                      </div>
                      <div class={styles.field}>
                        <label>Spec</label>
                        <Area class="input" rows={3} max={200} value={eSpec} onInput={(e) => setESpec((e.target as HTMLTextAreaElement).value)} />
                      </div>
                      <div class={styles.field}>
                        <label>Priority</label>
                        <input class="input" type="number" min={0} max={10} value={ePriority} onInput={(e) => setEPriority(Number((e.target as HTMLInputElement).value))} />
                      </div>
                      <div class={styles.field}>
                        <label>Worker role</label>
                        <select class="input" value={eRole} onChange={(e) => setERole((e.target as HTMLSelectElement).value)}>
                          <option value="">worker's own role</option>
                          {(leafRoles ?? []).map((r) => <option key={r} value={r}>{r}</option>)}
                        </select>
                      </div>
                      <div class={styles.field}>
                        <label>Status</label>
                        <select class="input" value={eStatus} onChange={(e) => setEStatus((e.target as HTMLSelectElement).value as Status)}>
                          {statuses.map((s) => <option key={s} value={s}>{s}</option>)}
                        </select>
                      </div>
                      <div class={styles.field}>
                        <label>Result</label>
                        <Area class="input" rows={3} max={200} value={eResult} onInput={(e) => setEResult((e.target as HTMLTextAreaElement).value)} />
                      </div>
                      <div class={detail.actions}>
                        <button type="button" class={`${detail.action} ${detail.primary}`} disabled={busy} onClick={() => void save(t)}>Save</button>
                        <button type="button" class={`${detail.action} ${detail.secondary}`} onClick={() => setEditing(null)}>Cancel</button>
                      </div>
                    </>
                  ) : (
                    <>
                      <p class={styles.meta}>
                        {ago(t.created_at)}
                        {t.claimed_by ? ` · claimed by ${shortId(t.claimed_by)}` : ''}
                        {t.role ? ` · runs as ${t.role}` : ''}
                      </p>
                      {t.spec && <p class={styles.pre}>{t.spec}</p>}
                      {t.result && <p class={styles.pre}>{t.result}</p>}
                      <div class={detail.actions}>
                        <button type="button" class={`${detail.action} ${detail.secondary}`} onClick={() => startEdit(t)}>Edit</button>
                        <button type="button" class={`${detail.action} ${detail.danger}`} onClick={() => void remove(t)}>
                          <Icon name="trash" size={14} /> Delete
                        </button>
                      </div>
                    </>
                  )}
                </div>
              </details>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
