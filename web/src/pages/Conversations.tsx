import { useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get } from '../api/client';
import { ago } from '../format';
import { shortId } from '../journal';
import { useLive } from '../live';
import { Chat } from './Chat';
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
  const [collapsed, setCollapsed] = useState(readCollapsed);
  const collapse = (c: boolean) => {
    setCollapsed(c);
    writeCollapsed(c);
  };
  return (
    <div class={`${styles.split} ${id ? styles.hasChat : ''} ${collapsed ? styles.collapsed : ''}`}>
      {collapsed && (
        <div class={styles.strip}>
          <button
            type="button"
            class={`button ${styles.fold}`}
            aria-label="Show conversations"
            title="Show conversations"
            aria-expanded="false"
            onClick={() => collapse(false)}
          >
            ›
          </button>
        </div>
      )}
      <List current={id} reloadKey={renamed} onCollapse={() => collapse(true)} />
      <section class={styles.pane} aria-label="Conversation">
        {id ? (
          <Chat key={id} id={id} onRenamed={() => setRenamed((n) => n + 1)} />
        ) : (
          <p class={`card muted ${styles.placeholder}`}>Choose a conversation, or start a new one.</p>
        )}
      </section>
    </div>
  );
}

// Whether the list is folded away is a per-browser preference: open unless
// this browser folded it. Storage can be missing or refuse, which reads as open.
const COLLAPSED_KEY = 'nine.conversations.collapsed';

function readCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSED_KEY) === '1';
  } catch {
    return false;
  }
}

function writeCollapsed(c: boolean) {
  try {
    if (c) localStorage.setItem(COLLAPSED_KEY, '1');
    else localStorage.removeItem(COLLAPSED_KEY);
  } catch {
    // Not remembered; the page still folds.
  }
}

function List({ current, reloadKey, onCollapse }: { current?: string; reloadKey: number; onCollapse: () => void }) {
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
    // Interactive, as the TUI's are: the session may ask the operator questions.
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
    <nav class={styles.list} aria-label="Conversations">
      <div class={styles.top}>
        <button type="button" class={`button ${styles.new}`} onClick={() => void start()} disabled={busy}>
          New conversation
        </button>
        <button
          type="button"
          class={`button ${styles.fold}`}
          aria-label="Hide conversations"
          title="Hide conversations"
          aria-expanded="true"
          onClick={onCollapse}
        >
          ‹
        </button>
      </div>
      {error && <p class="error">{error}</p>}
      {loadError && <p class="error">{loadError}</p>}
      {!data && !loadError && <p class="muted">Loading…</p>}
      {data && data.length === 0 && <p class="muted">No conversations yet.</p>}
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
