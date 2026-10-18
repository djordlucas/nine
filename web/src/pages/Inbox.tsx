import { useEffect, useState } from 'preact/hooks';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
import { attentionEvents, loadAttention, scopeOf, type CapabilityRequest, type Grant } from '../inbox';
import { shortId } from '../journal';
import { useLive } from '../live';
import { ask } from '../components/Confirm';
import detail from '../components/Detail.module.css';
import styles from './Inbox.module.css';
import { Icon } from '../components/Icon';

/**
 * The Inbox (adr/web-ui.md §5): what waits for the operator — capability
 * requests to approve or deny, questions a conversation asked — then the
 * notifications, and the capabilities in force.
 */
export function Inbox() {
  const { data, error } = useLive(loadAttention, attentionEvents);
  return (
    <div class={styles.inbox}>
      {error && <p class="card error">{error}</p>}
      <Requests requests={data?.requests} />
      <Questions data={data} />
      <Notifications />
      <Grants grants={data?.grants} />
    </div>
  );
}

function Requests({ requests }: { requests?: CapabilityRequest[] }) {
  return (
    <section class={`card ${detail.detail}`} aria-labelledby="requests-h">
      <h2 id="requests-h" class={styles.title}>
        Capability requests {requests && requests.length > 0 && <span class={styles.count}>{requests.length}</span>}
      </h2>
      {!requests && <p class="muted">Loading…</p>}
      {requests && requests.length === 0 && <p class="empty">No request is waiting.</p>}
      {requests && requests.length > 0 && (
        <ul class={styles.items}>
          {requests.map((r) => (
            <Request key={r.id} r={r} />
          ))}
        </ul>
      )}
    </section>
  );
}

function Request({ r }: { r: CapabilityRequest }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const decide = async (action: 'approve' | 'deny') => {
    if (action === 'approve' && !(await ask({ title: `Grant ${r.capability}?`, message: `Scope: ${scopeOf(r.params)}. The tools Nine writes may use it.`, confirm: 'Approve' }))) return;
    setBusy(true);
    setError(null);
    const { data, error: e } = await api.POST('/capabilities/{id}/decision', {
      params: { path: { id: r.id } },
      body: { action },
    });
    setBusy(false);
    if (!data) setError(errorMessage(e, 'Could not record the decision.'));
  };
  return (
    <li class={styles.item}>
      <p class={styles.what}>
        {r.tool_name ? (
          <>
            <a href={`/tools/${encodeURIComponent(r.tool_name)}`}>{r.tool_name}</a> wants{' '}
          </>
        ) : (
          'Nine wants '
        )}
        <code>{r.capability}</code> <span class="muted">({scopeOf(r.params)})</span>
      </p>
      {r.reason && <p class={styles.reason}>“{r.reason}”</p>}
      <p class={`muted ${styles.meta}`}>
        {r.agent_id && (
          <>
            from <a href={`/conversations/${r.agent_id}`}>{shortId(r.agent_id)}</a> ·{' '}
          </>
        )}
        {ago(r.created_at)}
      </p>
      <div class={detail.actions}>
        <button type="button" class={`${detail.action} ${detail.primary}`} disabled={busy} onClick={() => void decide('approve')}>
          Approve
        </button>
        <button type="button" class={`${detail.action} ${detail.danger}`} disabled={busy} onClick={() => void decide('deny')}>
          Deny
        </button>
      </div>
      {error && <p class="error">{error}</p>}
    </li>
  );
}

function Questions({ data }: { data: Awaited<ReturnType<typeof loadAttention>> | null }) {
  const qs = data?.questions;
  return (
    <section class={`card ${detail.detail}`} aria-labelledby="questions-h">
      <h2 id="questions-h" class={styles.title}>
        Questions {qs && qs.length > 0 && <span class={styles.count}>{qs.length}</span>}
      </h2>
      {!qs && <p class="muted">Loading…</p>}
      {qs && qs.length === 0 && <p class="empty">No conversation is waiting for an answer.</p>}
      {qs && qs.length > 0 && (
        <ul class={styles.items}>
          {qs.map((q) => (
            <li key={q.id} class={styles.item}>
              <p class={styles.what}>{q.question}</p>
              {q.options && q.options.length > 0 && <p class="muted">Options: {q.options.join(' · ')}</p>}
              <p class={`muted ${styles.meta}`}>
                asked {ago(q.created_at)}, expires {new Date(q.expires_at).toLocaleTimeString()}
              </p>
              <div class={detail.actions}>
                <a class={`${detail.action} ${detail.primary} ${styles.linkButton}`} href={`/conversations/${q.agent_id}`}>
                  Answer in the conversation
                </a>
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function Notifications() {
  const [marked, setMarked] = useState(0);
  const { data, error } = useLive(
    async () => {
      const list =
        (await get(api.GET('/notifications', { params: { query: { all: true, limit: 1000 } } }), 'notifications')).data ?? [];
      return [...list].sort((a, b) => (b.created_at ?? '').localeCompare(a.created_at ?? '')).slice(0, 100);
    },
    ['notification'],
    marked,
  );
  const unseen = data?.filter((n) => !n.seen).length ?? 0;
  // Opening the Inbox is reading it: what it shows as new is marked seen,
  // and stays highlighted until the page is left.
  useEffect(() => {
    if (unseen > 0) void api.GET('/notifications', { params: { query: { limit: 1000 } } });
  }, [unseen > 0]);
  return (
    <section class={`card ${detail.detail}`} aria-labelledby="notifications-h">
      <header class={detail.head}>
        <h2 id="notifications-h" class={styles.title}>
          Notifications {unseen > 0 && <span class={styles.count}>{unseen} new</span>}
        </h2>
        <button type="button" class={`${detail.action} ${detail.iconAction}`} onClick={() => setMarked((n) => n + 1)} aria-label="Refresh" title="Refresh">
          <Icon name="reload" size={18} />
        </button>
      </header>
      {error && <p class="error">{error}</p>}
      {!data && !error && <p class="muted">Loading…</p>}
      {data && data.length === 0 && <p class="empty">No notifications.</p>}
      {data && data.length > 0 && (
        <ul class={styles.notes}>
          {data.map((n) => (
            <li key={n.id} class={n.seen ? undefined : styles.unseen}>
              <span class={styles.note}>{n.message}</span>
              <span class={`muted ${styles.meta}`}>
                {n.agent_id && (
                  <>
                    <a href={`/conversations/${n.agent_id}`}>{shortId(n.agent_id)}</a> ·{' '}
                  </>
                )}
                {ago(n.created_at)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

const sourceLabels: Record<string, string> = {
  default: 'from the workspace root',
  config: 'from nine.toml',
  approved: 'approved by you',
};

function Grants({ grants }: { grants?: Grant[] }) {
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const revoke = async (g: Grant) => {
    if (!(await ask({ title: `Revoke ${g.capability}?`, message: `Scope: ${scopeOf(g.params)}. Tools that use it lose it.`, confirm: 'Revoke', danger: true }))) return;
    setBusy(g.id);
    setError(null);
    const { data, error: e } = await api.POST('/capabilities/{id}/decision', {
      params: { path: { id: g.id } },
      body: { action: 'revoke' },
    });
    setBusy(null);
    if (!data) setError(errorMessage(e, 'Could not revoke it.'));
  };
  return (
    <section class={`card ${detail.detail}`} aria-labelledby="grants-h">
      <h2 id="grants-h" class={styles.title}>
        Capabilities in force
      </h2>
      <p class="muted">What the tools Nine writes may use. Only grants you approved can be revoked here.</p>
      {error && <p class="error">{error}</p>}
      {!grants && <p class="muted">Loading…</p>}
      {grants && grants.length === 0 && <p class="muted">None.</p>}
      {grants && grants.length > 0 && (
        <ul class={styles.notes}>
          {grants.map((g) => (
            <li key={g.id}>
              <span class={styles.note}>
                <code>{g.capability}</code> <span class="muted">({scopeOf(g.params)})</span>
              </span>
              <span class={`muted ${styles.meta}`}>{sourceLabels[g.source] ?? g.source}</span>
              {g.source === 'approved' && (
                <button type="button" class={`${detail.action} ${detail.danger}`} disabled={busy === g.id} onClick={() => void revoke(g)}>
                  Revoke
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
