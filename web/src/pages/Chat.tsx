import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { api, errorMessage, get, type Schemas } from '../api/client';
import { ConversationSocket, type SocketState } from '../api/socket';
import { answered, apply, empty, fromHistory, pendingQuestions, reconcile, sent, type Chat as ChatState, type Item } from '../chat';
import { pretty } from '../format';
import { shortId } from '../journal';
import { Markdown } from '../markdown';
import styles from './Chat.module.css';

/** The most history entries the API returns in one page. */
const HISTORY_PAGE = 1000;

/** The whole transcript, or its last HISTORY_PAGE entries. */
async function loadHistory(id: string): Promise<Schemas['HistoryEntry'][]> {
  const first = await get(
    api.GET('/conversations/{id}/history', { params: { path: { id }, query: { limit: HISTORY_PAGE } } }),
    'the transcript',
  );
  const total = first.pagination?.total ?? 0;
  if (total <= HISTORY_PAGE) return first.data ?? [];
  const last = await get(
    api.GET('/conversations/{id}/history', {
      params: { path: { id }, query: { limit: HISTORY_PAGE, offset: total - HISTORY_PAGE } },
    }),
    'the transcript',
  );
  return last.data ?? [];
}

/** Whether Enter should send: on a keyboard, yes; on a touch screen it is a newline. */
const enterSends = () => !matchMedia('(pointer: coarse)').matches;

/**
 * One conversation: its transcript, then each turn live over its WebSocket,
 * whoever started it. Send a turn, answer the session's questions, cancel a
 * running turn.
 */
export function Chat({ id }: { id: string }) {
  const { route } = useLocation();
  const [info, setInfo] = useState<Schemas['ConversationInfo'] | null>(null);
  const [missing, setMissing] = useState(false);
  const [chat, setChat] = useState<ChatState>(empty);
  const current = useRef(chat);
  current.current = chat;
  const [loadError, setLoadError] = useState<string | null>(null);
  const [sock, setSock] = useState<SocketState>('connecting');
  const [panel, setPanel] = useState(false);
  const socket = useRef<ConversationSocket | null>(null);
  const bottom = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  // Reads of the transcript that came back without the latest turn, in a row.
  const behind = useRef(0);
  const load = useCallback(async () => {
    try {
      const [history, questions] = await Promise.all([
        loadHistory(id),
        get(api.GET('/questions'), 'questions'),
      ]);
      // Merged against the chat as it is now, not in a state updater: Preact
      // runs those later, and whether to read again is decided here.
      const merged = reconcile(fromHistory(history), current.current.items);
      for (const q of pendingQuestions(questions.data, id)) {
        if (!merged.items.some((i) => i.kind === 'question' && i.id === q.id)) merged.items.push(q);
      }
      setChat((c) => ({ items: merged.items, busy: c.busy }));
      setLoadError(null);
      // The journal lags the turn by a moment; read again until it has it.
      if (!merged.complete && behind.current < 5) {
        behind.current++;
        setTimeout(() => void loadRef.current(), 300 * behind.current);
      } else {
        behind.current = 0;
      }
    } catch (e) {
      setLoadError((e as Error).message);
    }
  }, [id]);
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    let alive = true;
    let s: ConversationSocket | null = null;
    // The socket opens only once the conversation is known to exist: a
    // handshake for a missing one is refused, and the browser reports that as
    // an error of the page's own.
    void api.GET('/conversations/{id}', { params: { path: { id } } }).then(({ data, response }) => {
      if (!alive) return;
      if (response.status === 404) {
        setMissing(true);
        return;
      }
      if (data) setInfo(data);
      void load();
      s = new ConversationSocket(id, {
        event: (ev) => {
          if (ev.type === 'connected' || ev.type === 'human_input_answered' || ev.type === 'cancel') return;
          setChat((c) => apply(c, ev));
          // The turn is over: the transcript now has it, with its number.
          if (ev.type === 'done') void load();
        },
        state: setSock,
        reopened: () => void load(),
      });
      socket.current = s;
    });
    return () => {
      alive = false;
      s?.close();
      socket.current = null;
    };
  }, [id, load]);

  // Follow the conversation as it grows, unless the operator scrolled up.
  useLayoutEffect(() => {
    if (stick.current) bottom.current?.scrollIntoView({ block: 'end' });
  }, [chat.items]);
  useEffect(() => {
    const onScroll = () => {
      const b = bottom.current?.getBoundingClientRect();
      if (b) stick.current = b.bottom <= window.innerHeight + 120;
    };
    window.addEventListener('scroll', onScroll, true);
    return () => window.removeEventListener('scroll', onScroll, true);
  }, []);

  const send = (text: string, think: boolean): boolean => {
    if (!socket.current?.send({ type: 'user_turn', text, force_think: think || undefined })) return false;
    stick.current = true;
    setChat((c) => sent(c, text));
    return true;
  };
  const answer = (q: Extract<Item, { kind: 'question' }>, a: string) => {
    if (socket.current?.send({ type: 'human_input_answer', request_id: q.id, answer: a })) setChat((c) => answered(c, q.id, a));
  };
  const cancel = () => socket.current?.send({ type: 'cancel' });

  const endSession = async () => {
    if (!confirm('End this session? Its worker stops and its checkpoint is deleted; the transcript stays.')) return;
    const { error } = await api.POST('/conversations/{id}/stop', { params: { path: { id } } });
    if (error) alert(errorMessage(error));
    else void load();
  };
  const remove = async () => {
    if (!confirm('Delete this conversation and everything recorded for it? This cannot be undone.')) return;
    const { error } = await api.DELETE('/conversations/{id}', { params: { path: { id } } });
    if (error) alert(errorMessage(error));
    else route('/conversations');
  };

  if (missing) {
    return (
      <div class="card">
        <p>This conversation does not exist.</p>
        <a href="/conversations">All conversations</a>
      </div>
    );
  }

  const title = info?.name || shortId(id);
  return (
    <div class={styles.chat}>
      <header class={styles.head}>
        <a class={styles.back} href="/conversations" aria-label="All conversations">
          ‹
        </a>
        <div class={styles.title}>
          <h2>{title}</h2>
          <span class="muted">
            {[info?.role, sock === 'open' ? '' : sock === 'connecting' ? 'connecting…' : 'reconnecting…'].filter(Boolean).join(' · ')}
          </span>
        </div>
        <div class={styles.actions}>
          <button type="button" class={styles.action} aria-expanded={panel} onClick={() => setPanel(!panel)}>
            Context
          </button>
          <button type="button" class={styles.action} onClick={() => void endSession()}>
            End session
          </button>
          <button type="button" class={`${styles.action} ${styles.danger}`} onClick={() => void remove()}>
            Delete
          </button>
        </div>
      </header>

      {panel && <ContextPanel id={id} />}
      {loadError && <p class="error">{loadError}</p>}

      <ol class={styles.timeline} aria-label="Messages">
        {chat.items.length === 0 && !loadError && <li class="muted">No messages yet. Say something.</li>}
        {chat.items.map((item, i) => (
          <li key={i} class={styles[item.kind]}>
            <ItemView item={item} id={id} onAnswer={answer} />
          </li>
        ))}
        {chat.busy && !chat.items.some((i) => i.kind === 'reply' && i.streaming) && (
          <li class={styles.working} aria-live="polite">
            Working…
          </li>
        )}
      </ol>
      <div ref={bottom} />

      <Composer busy={chat.busy} connected={sock === 'open'} onSend={send} onCancel={cancel} />
    </div>
  );
}

function ItemView({
  item,
  id,
  onAnswer,
}: {
  item: Item;
  id: string;
  onAnswer: (q: Extract<Item, { kind: 'question' }>, a: string) => void;
}) {
  switch (item.kind) {
    case 'user':
      return (
        <div class={styles.bubble} data-pending={item.pending || undefined}>
          {item.text}
        </div>
      );
    case 'reply':
      return (
        <div class={styles.reply}>
          <Markdown text={item.text} class={styles.md} />
          {item.streaming && <span class={styles.caret} aria-hidden="true" />}
          {item.turn !== undefined && !item.streaming && (
            <a class={styles.inspect} href={`/conversations/${id}/turns/${item.turn}`}>
              Inspect turn {item.turn}
            </a>
          )}
        </div>
      );
    case 'tool':
      return (
        <details class={styles.tool}>
          <summary>
            <span class={styles.toolName}>{item.name}</span>
            <span class="muted">{item.done ? 'done' : 'running…'}</span>
          </summary>
          {item.input !== undefined && item.input !== null && <pre class={styles.pre}>{pretty(item.input)}</pre>}
          {item.output !== undefined && <pre class={styles.pre}>{item.output}</pre>}
        </details>
      );
    case 'agent':
      return (
        <div class={styles.agentLine}>
          Sub-agent {item.role ? <strong>{item.role}</strong> : shortId(item.id)}: {item.task ?? ''}{' '}
          <span class="muted">{item.status ?? 'running…'}</span>
        </div>
      );
    case 'question':
      return <Question q={item} onAnswer={onAnswer} />;
    case 'notice':
      return <p class="muted">{item.text}</p>;
    case 'error':
      return <p class={item.cancelled ? 'muted' : 'error'}>{item.text}</p>;
  }
}

function Question({
  q,
  onAnswer,
}: {
  q: Extract<Item, { kind: 'question' }>;
  onAnswer: (q: Extract<Item, { kind: 'question' }>, a: string) => void;
}) {
  const [text, setText] = useState('');
  return (
    <div class={`card ${styles.question}`} role="group" aria-label="Question from the session">
      <p class={styles.qText}>{q.question}</p>
      {q.answered !== undefined ? (
        <p class="muted">Answered: {q.answered}</p>
      ) : (
        <>
          {q.options.length > 0 && (
            <div class={styles.options}>
              {q.options.map((o) => (
                <button key={o} type="button" class="button" onClick={() => onAnswer(q, o)}>
                  {o}
                </button>
              ))}
            </div>
          )}
          <form
            class={styles.qForm}
            onSubmit={(e) => {
              e.preventDefault();
              if (text.trim()) onAnswer(q, text.trim());
            }}
          >
            <input
              class="input"
              aria-label="Your answer"
              placeholder={q.options.length ? 'Or answer in your own words' : 'Your answer'}
              value={text}
              onInput={(e) => setText((e.target as HTMLInputElement).value)}
            />
            <button type="submit" class="button" disabled={!text.trim()}>
              Answer
            </button>
          </form>
        </>
      )}
    </div>
  );
}

function Composer({
  busy,
  connected,
  onSend,
  onCancel,
}: {
  busy: boolean;
  connected: boolean;
  onSend: (text: string, think: boolean) => boolean;
  onCancel: () => void;
}) {
  const [text, setText] = useState('');
  const [think, setThink] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const area = useRef<HTMLTextAreaElement>(null);

  const grow = () => {
    const a = area.current;
    if (!a) return;
    a.style.height = 'auto';
    a.style.height = `${Math.min(a.scrollHeight, 200)}px`;
  };
  const submit = () => {
    const t = text.trim();
    if (!t) return;
    if (!onSend(t, think)) {
      setError('Not connected yet; try again in a moment.');
      return;
    }
    setError(null);
    setText('');
    setThink(false);
    requestAnimationFrame(grow);
  };

  return (
    <form
      class={styles.composer}
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      {error && <p class="error">{error}</p>}
      <textarea
        ref={area}
        class={styles.input}
        rows={1}
        aria-label="Message"
        placeholder={busy ? 'A message now waits for the turn to finish' : 'Message'}
        value={text}
        onInput={(e) => {
          setText((e.target as HTMLTextAreaElement).value);
          grow();
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && enterSends()) {
            e.preventDefault();
            submit();
          }
        }}
      />
      <div class={styles.controls}>
        <label class={styles.think}>
          <input type="checkbox" checked={think} onChange={(e) => setThink((e.target as HTMLInputElement).checked)} />
          Think
        </label>
        {busy && (
          <button type="button" class={`button ${styles.stop}`} onClick={onCancel}>
            Stop
          </button>
        )}
        <button type="submit" class="button" disabled={!text.trim() || !connected}>
          Send
        </button>
      </div>
    </form>
  );
}

/** What the session's next model call would be shown, by section. */
function ContextPanel({ id }: { id: string }) {
  const [report, setReport] = useState<ContextReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    api.GET('/conversations/{id}/context', { params: { path: { id } } }).then(({ data, error: e }) => {
      if (data?.context) setReport(data.context as unknown as ContextReport);
      else setError(errorMessage(e, 'Could not load the context.'));
    });
  }, [id]);
  return (
    <section class={`card ${styles.context}`} aria-label="Context">
      {error && <p class="error">{error}</p>}
      {!report && !error && <p class="muted">Loading…</p>}
      {report && (
        <>
          <p>
            {report.used ?? 0} of {report.budget ?? 0} tokens
          </p>
          <table class={styles.sections}>
            <tbody>
              {(report.sections ?? []).map((s) => (
                <tr key={s.name} class={s.included ? '' : 'muted'}>
                  <th scope="row">{s.name}</th>
                  <td>{s.included ? s.tokens : 'dropped'}</td>
                  <td class="muted">{s.detail}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {report.system && (
            <details>
              <summary>System prompt</summary>
              <pre class={styles.pre}>{report.system}</pre>
            </details>
          )}
        </>
      )}
    </section>
  );
}

/** The daemon's context report (internal/context Report). */
interface ContextReport {
  budget?: number;
  used?: number;
  system?: string;
  sections?: { name: string; priority?: string; tokens: number; included: boolean; detail?: string }[];
}
