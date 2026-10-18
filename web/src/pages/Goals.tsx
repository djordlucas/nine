import { useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get, type Schemas } from '../api/client';
import { ago } from '../format';
import { fromConfig, goalTree, statusActions, type Goal, type GoalStatus } from '../goals';
import { shortId } from '../journal';
import { useLive } from '../live';
import { processState } from '../processes';
import { ask } from '../components/Confirm';
import { ListHead, Split } from '../components/Split';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import { param, processPath } from './Processes';
import styles from './Goals.module.css';
import { Icon } from '../components/Icon';

type Workflow = Schemas['WorkflowInfo'];

const goalEvents = ['goal_*'];
const workflowEvents = ['workflow_*'];

/**
 * Goals and workflows (adr/web-ui.md §5): goals as a tree and the workflows
 * beside one open goal or workflow. Goals are created, paused, resumed,
 * finished and deleted here; a workflow can be stopped or failed.
 */
export function Goals() {
  const { params } = useRoute();
  const { path } = useLocation();
  const id = param(params.id as string | undefined);
  const isWorkflow = path.startsWith('/goals/workflows/');
  return (
    <Split name="goals" noun="goals and workflows" open={!!id}>
      <div class={split.list}>
        <GoalList current={isWorkflow ? undefined : id} />
        <WorkflowList current={isWorkflow ? id : undefined} />
      </div>
      <section class={split.pane} aria-label={isWorkflow ? 'Workflow' : 'Goal'}>
        {id && isWorkflow && <WorkflowDetail key={id} id={id} />}
        {id && !isWorkflow && <GoalDetail key={id} id={id} />}
        {!id && <p class={`card muted ${split.placeholder}`}>Choose a goal or a workflow, or create a goal.</p>}
      </section>
    </Split>
  );
}

function goalPath(id: string): string {
  return `/goals/${encodeURIComponent(id)}`;
}

async function loadGoals(): Promise<Goal[]> {
  return (await get(api.GET('/goals', { params: { query: { limit: 1000 } } }), 'goals')).data ?? [];
}

function GoalList({ current }: { current?: string }) {
  const { data, error } = useLive(loadGoals, goalEvents);
  const rows = data ? goalTree(data) : [];
  return (
    <nav class={styles.group} aria-label="Goals">
      <ListHead>
        <h2 class={styles.groupTitle}>Goals</h2>
      </ListHead>
      <NewGoal />
      {error && <p class="error">{error}</p>}
      {!data && !error && <p class="muted">Loading…</p>}
      {data && rows.length === 0 && <p class="muted">No goals yet.</p>}
      {rows.length > 0 && (
        <ul>
          {rows.map(({ goal: g, depth }) => (
            <li key={g.id}>
              <a
                href={goalPath(g.id ?? '')}
                class={split.row}
                style={{ paddingLeft: `calc(var(--space-2) + ${depth} * var(--space-4))` }}
                aria-current={g.id === current ? 'page' : undefined}
              >
                <span class={`${detail.badge} ${detail[g.status ?? '']}`}>{g.status}</span>
                <span class={split.name}>{g.description}</span>
              </a>
            </li>
          ))}
        </ul>
      )}
    </nav>
  );
}

/** A form that creates a goal: top-level, or a sub-goal of parent. */
function NewGoal({ parent }: { parent?: string }) {
  const { route } = useLocation();
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const create = async (e: Event) => {
    e.preventDefault();
    if (!text.trim()) return;
    setBusy(true);
    setError(null);
    const { data, error: err } = await api.POST('/goals', {
      body: parent ? { description: text, parent_id: parent } : { description: text },
    });
    setBusy(false);
    if (!data) {
      setError(errorMessage(err, 'Could not create the goal.'));
      return;
    }
    setText('');
    if (data.pursue_session === 'limit_reached') {
      setError('Created, but nothing pursues it yet: the daemon is at its process limit.');
    }
    if (!parent && data.goal.id) route(goalPath(data.goal.id));
  };
  const label = parent ? 'New sub-goal' : 'New goal';
  return (
    <form class={styles.create} onSubmit={(e) => void create(e)}>
      <input
        class="input"
        aria-label={label}
        placeholder={parent ? 'A step toward this goal' : 'What should Nine keep working on?'}
        value={text}
        onInput={(e) => setText(e.currentTarget.value)}
      />
      <button type="submit" class="button" disabled={busy || !text.trim()}>
        {parent ? 'Add' : 'Create'}
      </button>
      {error && <p class="error">{error}</p>}
    </form>
  );
}

function GoalDetail({ id }: { id: string }) {
  const { route } = useLocation();
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  const { data, error } = useLive(
    async () => {
      const [goals, procs] = await Promise.all([
        loadGoals(),
        get(api.GET('/processes', { params: { query: { limit: 1000 } } }), 'processes'),
      ]);
      return { goals, procs: (procs.data ?? []).filter((p) => p.goal === id) };
    },
    ['goal_*', 'process_*', 'standing_*'],
  );

  if (error) return <p class="card error">{error}</p>;
  if (!data) return <p class="card muted">Loading…</p>;
  const g = data.goals.find((x) => x.id === id);
  if (!g) return <p class="card muted">No goal {id}. It may have been deleted.</p>;
  const parent = g.parent_type === 'goal' ? data.goals.find((x) => x.id === g.parent_id) : undefined;
  const subs = data.goals.filter((x) => x.parent_type === 'goal' && x.parent_id === id);

  const setStatus = async (status: GoalStatus) => {
    setBusy(true);
    setOutcome(null);
    const { data: done, error: err } = await api.POST('/goals/{id}/status', {
      params: { path: { id } },
      body: { status },
    });
    setBusy(false);
    setOutcome(done ? { ok: true, text: `Now ${done.status}.` } : { ok: false, text: errorMessage(err, 'Could not change the status.') });
  };
  const remove = async () => {
    const what = subs.length ? `this goal and its ${subs.length} sub-goal(s)` : 'this goal';
    if (!(await ask({ title: `Delete ${what}?`, message: 'Its session stops; its history is kept.', confirm: 'Delete', danger: true }))) return;
    setBusy(true);
    const { data: done, error: err } = await api.DELETE('/goals/{id}', { params: { path: { id } } });
    setBusy(false);
    if (done) {
      route('/goals');
      return;
    }
    setOutcome({ ok: false, text: errorMessage(err, 'Could not delete the goal.') });
  };

  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href="/goals" aria-label="All goals">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{g.description}</h2>
          <span class="muted">
            <span class={`${detail.badge} ${detail[g.status ?? '']}`}>{g.status}</span> {shortId(id)}
          </span>
        </div>
        <div class={detail.actions}>
          {statusActions(g.status).map(([status, label], i) => (
            <button
              key={status}
              type="button"
              class={`${detail.action} ${i === 0 ? detail.primary : ''}`}
              disabled={busy}
              onClick={() => void setStatus(status)}
            >
              {label}
            </button>
          ))}
          {!fromConfig(g) && (
            <button type="button" class={`${detail.action} ${detail.iconAction} ${detail.danger}`} disabled={busy} onClick={() => void remove()} aria-label="Delete" title="Delete this goal">
              <Icon name="trash" size={18} />
            </button>
          )}
        </div>
      </header>

      {outcome && (
        <p class={outcome.ok ? detail.notice : 'error'} role="status">
          {outcome.text}
        </p>
      )}
      {fromConfig(g) && <p class={detail.notice}>Declared by a [[process]] block in nine.toml; remove the block to delete it.</p>}

      <dl class={detail.facts}>
        {parent && (
          <>
            <dt>Part of</dt>
            <dd>
              <a href={goalPath(parent.id ?? '')}>{parent.description}</a>
            </dd>
          </>
        )}
        <dt>Created</dt>
        <dd>{ago(g.created_at)}</dd>
        <dt>Updated</dt>
        <dd>{ago(g.updated_at)}</dd>
        <dt>Origin</dt>
        <dd>{g.parent_type}</dd>
      </dl>

      <section class={detail.section} aria-labelledby="procs-h">
        <h3 id="procs-h">Processes</h3>
        {data.procs.length === 0 && (
          <p class="muted">{g.parent_type === 'goal' ? 'Its parent goal’s session works on it.' : 'None is bound to it.'}</p>
        )}
        {data.procs.length > 0 && (
          <ul class={styles.plain}>
            {data.procs.map((p) => (
              <li key={p.id}>
                <span class={`${detail.badge} ${detail[processState(p)]}`}>{processState(p)}</span>{' '}
                <a href={processPath(p.id)}>{p.id}</a>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section class={detail.section} aria-labelledby="subs-h">
        <h3 id="subs-h">Sub-goals</h3>
        {subs.length === 0 && <p class="muted">None.</p>}
        {subs.length > 0 && (
          <ul class={styles.plain}>
            {subs.map((s) => (
              <li key={s.id}>
                <span class={`${detail.badge} ${detail[s.status ?? '']}`}>{s.status}</span>{' '}
                <a href={goalPath(s.id ?? '')}>{s.description}</a>
              </li>
            ))}
          </ul>
        )}
        <NewGoal parent={id} />
      </section>
    </article>
  );
}

async function loadWorkflows(): Promise<Workflow[]> {
  const list = (await get(api.GET('/workflows', { params: { query: { limit: 1000 } } }), 'workflows')).data ?? [];
  return [...list].sort(
    (a, b) => Number(b.status === 'active') - Number(a.status === 'active') || (b.updated_at ?? '').localeCompare(a.updated_at ?? ''),
  );
}

function WorkflowList({ current }: { current?: string }) {
  const { data, error } = useLive(loadWorkflows, workflowEvents);
  return (
    <nav class={styles.group} aria-label="Workflows">
      <h2 class={styles.groupTitle}>Workflows</h2>
      {error && <p class="error">{error}</p>}
      {!data && !error && <p class="muted">Loading…</p>}
      {data && data.length === 0 && <p class="muted">No workflows. The model starts one for a bounded, multi-step plan.</p>}
      {data && data.length > 0 && (
        <ul>
          {data.map((w) => (
            <li key={w.id}>
              <a
                href={`/goals/workflows/${encodeURIComponent(w.id ?? '')}`}
                class={split.row}
                aria-current={w.id === current ? 'page' : undefined}
              >
                <span class={`${detail.badge} ${detail[w.status ?? '']}`}>{w.status}</span>
                <span class={split.name}>{w.name}</span>
                <span class={split.meta}>
                  {w.completed_steps ?? 0} of {w.steps ?? 0} steps · {ago(w.updated_at)}
                </span>
              </a>
            </li>
          ))}
        </ul>
      )}
    </nav>
  );
}

function WorkflowDetail({ id }: { id: string }) {
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  const { data, error } = useLive(loadWorkflows, workflowEvents);
  if (error) return <p class="card error">{error}</p>;
  if (!data) return <p class="card muted">Loading…</p>;
  const w = data.find((x) => x.id === id);
  if (!w) return <p class="card muted">No workflow {id}.</p>;

  const end = async (how: 'stop' | 'fail') => {
    const q =
      how === 'stop'
        ? { title: 'Stop this workflow?', message: 'Its remaining steps are cancelled.', confirm: 'Stop' }
        : { title: 'Mark this workflow failed?', message: 'Its remaining steps are not run.', confirm: 'Mark failed' };
    if (!(await ask(q))) return;
    setBusy(true);
    setOutcome(null);
    const path = { params: { path: { id } } };
    const { data: done, error: err } =
      how === 'stop' ? await api.POST('/workflows/{id}/stop', path) : await api.POST('/workflows/{id}/fail', path);
    setBusy(false);
    setOutcome(done ? { ok: true, text: done.message ?? 'Done.' } : { ok: false, text: errorMessage(err, 'The daemon refused.') });
  };

  const steps = w.steps ?? 0;
  const doneSteps = w.completed_steps ?? 0;
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href="/goals" aria-label="All goals and workflows">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{w.name}</h2>
          <span class="muted">
            <span class={`${detail.badge} ${detail[w.status ?? '']}`}>{w.status}</span> {shortId(id)}
          </span>
        </div>
        {w.status === 'active' && (
          <div class={detail.actions}>
            <button type="button" class={`${detail.action} ${detail.iconAction}`} disabled={busy} onClick={() => void end('stop')} aria-label="Stop" title="Stop this workflow">
              <Icon name="stop" size={18} />
            </button>
            <button type="button" class={`${detail.action} ${detail.danger}`} disabled={busy} onClick={() => void end('fail')}>
              Mark failed
            </button>
          </div>
        )}
      </header>
      {outcome && (
        <p class={outcome.ok ? detail.notice : 'error'} role="status">
          {outcome.text}
        </p>
      )}
      <label class={styles.progress}>
        <span>
          {doneSteps} of {steps} steps done
        </span>
        <progress max={Math.max(steps, 1)} value={doneSteps} />
      </label>
      <dl class={detail.facts}>
        {w.agent_id && (
          <>
            <dt>Session</dt>
            <dd>
              <a href={`/conversations/${w.agent_id}`}>{shortId(w.agent_id)}</a>
            </dd>
          </>
        )}
        <dt>Started</dt>
        <dd>{ago(w.created_at)}</dd>
        <dt>Updated</dt>
        <dd>{ago(w.updated_at)}</dd>
      </dl>
    </article>
  );
}
