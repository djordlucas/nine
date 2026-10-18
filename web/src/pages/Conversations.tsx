import { useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
import { shortId } from '../journal';
import { useLive } from '../live';
import { Chat } from './Chat';
import { ListHead, Split } from '../components/Split';
import split from '../components/Split.module.css';
import styles from './Conversations.module.css';

/**
 * Conversations (adr/web-ui.md §5): the list beside the open chat on a desktop;
 * on a phone, the list, then the chat in its place.
 */
export function Conversations() {
  const { params } = useRoute();
  const id = params.id as string | undefined;
  // Bumped by a rename, so the list reads the new name.
  const [renamed, setRenamed] = useState(0);
  return (
    <Split name="conversations" noun="conversations" open={!!id}>
      <List current={id} reloadKey={renamed} />
      <section class={split.pane} aria-label="Conversation">
        {id ? (
          <Chat key={id} id={id} onRenamed={() => setRenamed((n) => n + 1)} />
        ) : (
          <p class="empty">Choose a conversation, or start a new one.</p>
        )}
      </section>
    </Split>
  );
}

function List({ current, reloadKey }: { current?: string; reloadKey: number }) {
  const { route } = useLocation();
  const [created, setCreated] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const { data, error: loadError } = useLive(
    async () => {
      const list = (await get(api.GET('/conversations', { params: { query: { limit: 200 } } }), 'conversations')).data ?? [];
      return [...list].sort((a, b) => (b.updated_at ?? '').localeCompare(a.updated_at ?? ''));
    },
    ['turn_start', 'turn_end'],
    `${created}:${reloadKey}`,
  );

  const start = async () => {
    setBusy(true);
    setError(null);
    // Interactive: the session may ask the operator questions.
    const { data: made, error: e } = await api.POST('/conversations', { body: { interactive: true } });
    setBusy(false);
    if (!made) {
      setError(errorMessage(e, 'Could not start a conversation.'));
      return;
    }
    setCreated((n) => n + 1);
    route(`/conversations/${made.id}`);
  };

  return (
    <nav class={split.list} aria-label="Conversations">
      <ListHead>
        <button type="button" class={`button ${styles.new}`} onClick={() => void start()} disabled={busy}>
          New conversation
        </button>
      </ListHead>
      {error && <p class="error">{error}</p>}
      {loadError && <p class="error">{loadError}</p>}
      {!data && !loadError && <p class="muted">Loading…</p>}
      {data && data.length === 0 && <p class="empty">No conversations yet.</p>}
      {data && data.length > 0 && (
        <ul>
          {data.map((c) => (
            <li key={c.id}>
              <a href={`/conversations/${c.id}`} class={styles.row} aria-current={c.id === current ? 'page' : undefined}>
                <span class={styles.name}>{c.name || shortId(c.id ?? '')}</span>
                <span class={styles.meta}>
                  {[c.role, c.status, ago(c.updated_at)].filter(Boolean).join(' · ')}
                </span>
              </a>
            </li>
          ))}
        </ul>
      )}
    </nav>
  );
}
