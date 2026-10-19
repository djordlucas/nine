import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { api, errorMessage, get, type Schemas } from '../api/client';
import { ConversationSocket, type SocketState } from '../api/socket';
import { answered, apply, empty, fromHistory, pendingQuestions, reconcile, sent, type Chat as ChatState, type Item } from '../chat';
import { shortId } from '../journal';
import { Markdown } from '../markdown';
import styles from './Chat.module.css';
import { Json } from '../components/Json';
import { Icon } from '../components/Icon';
import { fit } from '../components/Area';
import { ask } from '../components/Confirm';
import { copyText } from '../clipboard';
import { TurnDetail } from '../components/TurnDetail';
import detail from '../components/Detail.module.css';

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
/**
 * One session's chat. readOnly watches it without taking part: a process's
 * session, which its process drives, has no message box, rename or controls
 * beyond the context.
 */
export function Chat({ id, onRenamed, readOnly }: { id: string; onRenamed?: () => void; readOnly?: boolean }) {
  const { route } = useLocation();
  const [info, setInfo] = useState<Schemas['ConversationInfo'] | null>(null);
  const [missing, setMissing] = useState(false);
  const [chat, setChat] = useState<ChatState>(empty);
  const current = useRef(chat);
  current.current = chat;
  const [loadError, setLoadError] = useState<string | null>(null);
  const [sock, setSock] = useState<SocketState>('connecting');
  const [panel, setPanel] = useState(false);
  // The model that answers and its provider, from the daemon's status; empty
  // until they are known.
  const [llm, setLlm] = useState({ model: '', provider: '' });
  useEffect(() => {
    api.GET('/status').then(
      ({ data }) => setLlm({ model: data?.model ?? '', provider: data?.provider ?? '' }),
      () => undefined,
    );
  }, []);
  // How full the session's context is before any turn reports it.
  const [contextNow, setContextNow] = useState<{ used: number; budget: number } | undefined>();
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
      setChat((c) => ({ ...c, items: merged.items }));
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
    const refreshInfo = async () => {
      const { data: d } = await api.GET('/conversations/{id}', { params: { path: { id } } });
      if (alive && d) setInfo(d);
      // Until a turn reports its context use, the bar shows what the next
      // call would take.
      const { data: c } = await api.GET('/conversations/{id}/context', { params: { path: { id } } });
      const report = c?.context as unknown as ContextReport | undefined;
      if (alive && report?.budget) setContextNow({ used: report.used ?? 0, budget: report.budget });
    };
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
          // Connected, the session is loaded: its details now carry its plan mode.
          if (ev.type === 'connected') void refreshInfo();
          if (ev.type === 'connected' || ev.type === 'human_input_answered' || ev.type === 'cancel') return;
          setChat((c) => apply(c, ev));
          // The turn is over: the transcript now has it, with its number. The
          // first turn also names the conversation, so its details are read
          // again for the title.
          if (ev.type === 'done') {
            void load();
            void refreshInfo();
          }
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

  // The reply whose inspect button is under the pointer or focused, by its
  // place in the timeline; that reply is shaded, its buttons in the corner.
  const [inspected, setInspected] = useState<number | null>(null);
  // The turns whose record is open under their reply, by turn number.
  const [opened, setOpened] = useState<ReadonlySet<number>>(new Set());
  const toggleTurn = (turn: number) =>
    setOpened((o) => {
      const next = new Set(o);
      if (!next.delete(turn)) next.add(turn);
      return next;
    });
  const endSession = async () => {
    const ok = await ask({
      title: 'End this session?',
      message: 'Its worker stops and its checkpoint is deleted; the transcript stays.',
      confirm: 'End session',
    });
    if (!ok) return;
    const { error } = await api.POST('/conversations/{id}/stop', { params: { path: { id } } });
    if (error) alert(errorMessage(error));
    else void load();
  };
  const remove = async () => {
    const ok = await ask({
      title: 'Delete this conversation?',
      message: 'Everything recorded for it goes too. This cannot be undone.',
      confirm: 'Delete',
      danger: true,
    });
    if (!ok) return;
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
          {readOnly ? (
            <h2>{title}</h2>
          ) : (
            <Title
              id={id}
              title={title}
              onRenamed={(name) => {
                setInfo((i) => ({ ...(i ?? {}), name }));
                onRenamed?.();
              }}
            />
          )}
          <span class="muted">
            {[info?.role, sock === 'open' ? '' : sock === 'connecting' ? 'connecting…' : 'reconnecting…'].filter(Boolean).join(' · ')}
          </span>
        </div>
        <div class={styles.actions}>
          <button
            type="button"
            class={`${detail.action} ${detail.iconAction}`}
            aria-expanded={panel}
            aria-label="Context"
            title={panel ? 'Hide the context' : 'Context: what the next model call would be shown'}
            onClick={() => setPanel(!panel)}
          >
            <Icon name="context" size={16} />
          </button>
          {!readOnly && (
            <>
              <button type="button" class={`${detail.action} ${detail.iconAction}`} aria-label="End session" title="End session" onClick={endSession}>
                <Icon name="power" size={16} />
              </button>
              <button
                type="button"
                class={`${detail.action} ${detail.iconAction} ${detail.danger}`}
                aria-label="Delete"
                title="Delete this conversation"
                onClick={remove}
              >
                <Icon name="trash" size={16} />
              </button>
            </>
          )}
        </div>
      </header>

      {panel && <ContextPanel id={id} />}
      {loadError && <p class="error">{loadError}</p>}

      <ol class={styles.timeline} aria-label="Messages">
        {chat.items.length === 0 && !loadError && <li class="muted">No messages yet. Say something.</li>}
        {chat.items.map((item, i) => (
          <li key={i} class={styles[item.kind]} data-inspected={i === inspected || undefined}>
            <ItemView
              item={item}
              id={id}
              onAnswer={answer}
              onInspect={(on) => setInspected(on ? i : null)}
              opened={opened}
              onToggleTurn={toggleTurn}
            />
          </li>
        ))}
        {chat.busy && !chat.items.some((i) => i.kind === 'reply' && i.streaming) && (
          <li class={styles.working} aria-live="polite">
            {chat.stage ? `${chat.stage}…` : 'Working…'}
          </li>
        )}
      </ol>
      <div ref={bottom} />

      <footer class={styles.footer}>
        {!readOnly && (
          <Composer
            id={id}
            busy={chat.busy}
            connected={sock === 'open'}
            onSend={send}
            onCancel={cancel}
            planMode={info?.plan_mode}
            onPlanMode={(plan_mode) => setInfo((i) => ({ ...(i ?? {}), plan_mode }))}
          />
        )}
        <SessionBar model={llm.model} provider={llm.provider} context={chat.context ?? contextNow} />
      </footer>
    </div>
  );
}

/**
 * The conversation's name, which a click turns into a field: Enter or leaving
 * the field saves it, Escape keeps the old one.
 */
function Title({ id, title, onRenamed }: { id: string; title: string; onRenamed: (name: string) => void }) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(title);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const field = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (editing) field.current?.select();
  }, [editing]);

  const save = async () => {
    const name = draft.trim();
    if (saving) return;
    if (!name || name === title) {
      setEditing(false);
      setError(null);
      return;
    }
    setSaving(true);
    const { data, error: e } = await api.PATCH('/conversations/{id}', { params: { path: { id } }, body: { name } });
    setSaving(false);
    if (!data) {
      setError(errorMessage(e, 'Could not rename the conversation.'));
      field.current?.focus();
      return;
    }
    setError(null);
    setEditing(false);
    onRenamed(data.name);
  };

  if (!editing) {
    return (
      <h2>
        <button
          type="button"
          class={styles.rename}
          title="Rename"
          aria-label={`${title}, rename`}
          onClick={() => {
            setDraft(title);
            setEditing(true);
          }}
        >
          {title}
        </button>
      </h2>
    );
  }
  return (
    <div class={styles.renaming}>
      <input
        ref={field}
        class={styles.nameField}
        aria-label="Conversation name"
        maxLength={80}
        value={draft}
        disabled={saving}
        onInput={(e) => setDraft((e.target as HTMLInputElement).value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault();
            void save();
          } else if (e.key === 'Escape') {
            setEditing(false);
            setError(null);
          }
        }}
        onBlur={() => void save()}
      />
      {error && (
        <span class="error" role="alert">
          {error}
        </span>
      )}
    </div>
  );
}

/**
 * A turn's inspector: it opens the turn's record under the reply, boxed like
 * an open tool call, and closes it again. Which turns are open is kept by turn
 * number in the chat, so a reload of the transcript that moves the reply in
 * the list does not close it.
 */
function InspectButton({
  turn,
  open,
  onToggle,
  onInspect,
}: {
  turn: number;
  open: boolean;
  onToggle: () => void;
  onInspect: (on: boolean) => void;
}) {
  return (
    <button
      type="button"
      class={styles.inspect}
      aria-expanded={open}
      aria-label={`Inspect turn ${turn}`}
      title={open ? `Close turn ${turn}` : `Inspect turn ${turn}`}
      onClick={onToggle}
      onMouseEnter={() => onInspect(true)}
      onMouseLeave={() => onInspect(false)}
      onFocus={() => onInspect(true)}
      onBlur={() => onInspect(false)}
    >
      <Icon name="inspect" size={12} />
    </button>
  );
}

/**
 * Copies a message's text, as Markdown, to the clipboard. It answers the way
 * the UI's other buttons do, by its shade: dark for a moment when the copy
 * went through, red when the browser refused it.
 */
function CopyMd({ text, what }: { text: string; what: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  useEffect(() => {
    if (state === 'idle') return;
    // About as long as a click: a flash, not a state.
    const t = setTimeout(() => setState('idle'), 150);
    return () => clearTimeout(t);
  }, [state]);
  return (
    <button
      type="button"
      class={styles.copy}
      data-state={state === 'idle' ? undefined : state}
      title={`Copy ${what} as Markdown`}
      aria-label={`Copy ${what} as Markdown`}
      onClick={() => void copyText(text).then((ok) => setState(ok ? 'copied' : 'failed'))}
    >
      MD
    </button>
  );
}

function ItemView({
  item,
  id,
  onAnswer,
  onInspect,
  opened,
  onToggleTurn,
}: {
  item: Item;
  id: string;
  onAnswer: (q: Extract<Item, { kind: 'question' }>, a: string) => void;
  onInspect: (on: boolean) => void;
  opened: ReadonlySet<number>;
  onToggleTurn: (turn: number) => void;
}) {
  switch (item.kind) {
    case 'user':
      return (
        <div class={styles.userRow}>
          <div class={styles.bubble} data-pending={item.pending || undefined}>
            {item.text}
          </div>
          <div class={styles.side}>
            <CopyMd text={item.text} what="your message" />
          </div>
        </div>
      );
    case 'reply':
      return (
        <div class={styles.reply}>
          <div class={styles.replyBody}>
            <Markdown text={item.text} class={styles.md} />
            {item.streaming && <span class={styles.caret} aria-hidden="true" />}
            {item.turn !== undefined && opened.has(item.turn) && (
              <div class={styles.turnBox}>
                <TurnDetail id={id} n={item.turn} />
              </div>
            )}
          </div>
          {!item.streaming && (
            <div class={styles.side}>
              <CopyMd text={item.text} what="the reply" />
              {item.turn !== undefined && (
                <InspectButton
                  turn={item.turn}
                  open={opened.has(item.turn)}
                  onToggle={() => onToggleTurn(item.turn!)}
                  onInspect={onInspect}
                />
              )}
            </div>
          )}
        </div>
      );
    case 'tool':
      return (
        <details class={styles.toolBox}>
          <summary>
            <span class={styles.toolName}>{item.name}</span>
            <span class="muted">{item.done ? 'done' : 'running…'}</span>
          </summary>
          {item.input !== undefined && item.input !== null && <Json value={item.input} class={styles.pre} />}
          {item.output !== undefined && <Json value={item.output} class={styles.pre} />}
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
    case 'thinking':
      // Open while the model reasons; folded once its call moves on.
      return (
        <details class={styles.thinkingBox} open={!item.done}>
          <summary>
            {item.done ? 'Reasoning' : 'Reasoning…'}
            {item.step > 0 && <span class="muted"> · call {item.step}</span>}
          </summary>
          <p class={styles.thinkingText}>{item.text}</p>
        </details>
      );
    case 'notice':
      return <p class="muted">{item.text}</p>;
    case 'error':
      if (item.cancelled) return <p class="muted">{item.text}</p>;
      return (
        <div class={styles.failed} role="alert">
          <strong>The turn failed</strong>
          <p>{item.text}</p>
        </div>
      );
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
      {q.origin && <p class="muted">From {q.origin}</p>}
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

/** The tallest the message box grows before it scrolls, in px. */
const MAX_INPUT = 200;

function Composer({
  id,
  busy,
  connected,
  onSend,
  onCancel,
  planMode,
  onPlanMode,
}: {
  id: string;
  busy: boolean;
  connected: boolean;
  onSend: (text: string, think: boolean) => boolean;
  onCancel: () => void;
  planMode?: string;
  onPlanMode: (mode: string) => void;
}) {
  const [text, setText] = useState('');
  const [think, setThink] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const area = useRef<HTMLTextAreaElement>(null);

  const grow = () => fit(area.current, MAX_INPUT);
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
      <div class={styles.entry}>
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
        <div class={styles.send}>
          {busy && (
            <button type="button" class={`button ${styles.press} ${styles.stop}`} onClick={onCancel}>
              Stop
            </button>
          )}
          <button type="submit" class={`button ${styles.press}`} disabled={!text.trim() || !connected}>
            Send
          </button>
          <button
            type="button"
            class={styles.think}
            aria-pressed={think}
            aria-label="Think"
            title={think ? 'Think: on for the next message' : 'Think: reason before answering the next message'}
            onClick={() => setThink(!think)}
          >
            T
          </button>
          {planMode && <PlanButton id={id} mode={planMode} onChange={onPlanMode} onError={setError} />}
        </div>
      </div>
    </form>
  );
}

/** The plan modes a session can run in, in the order a click steps through
 * them (docs/thinking-and-planning.md). */
const PLAN_MODES = ['off', 'plan-only', 'always'] as const;

const PLAN_HELP: Record<string, string> = {
  off: 'Plan: off. The session acts without a plan first',
  'plan-only': 'Plan: plan-only. The session plans when the task calls for it',
  always: 'Plan: always. The session plans before every turn',
};

/**
 * The session's plan mode, beside Think and shaped like it: a click moves to
 * the next mode, for this session only. Light when off, edged when plan-only,
 * dark when always.
 */
function PlanButton(props: { id: string; mode: string; onChange: (mode: string) => void; onError: (e: string | null) => void }) {
  const [busy, setBusy] = useState(false);
  const at = PLAN_MODES.indexOf(props.mode as (typeof PLAN_MODES)[number]);
  const next = PLAN_MODES[(at + 1) % PLAN_MODES.length];
  const change = async () => {
    setBusy(true);
    props.onError(null);
    const { data, error: e } = await api.POST('/conversations/{id}/plan-mode', {
      params: { path: { id: props.id } },
      body: { mode: next },
    });
    setBusy(false);
    if (data) props.onChange(data.plan_mode);
    else props.onError(errorMessage(e, 'Could not change the plan mode.'));
  };
  return (
    <button
      type="button"
      class={`${styles.think} ${styles.planMode}`}
      data-mode={props.mode}
      disabled={busy}
      aria-label={`Plan: ${props.mode}`}
      title={`${PLAN_HELP[props.mode] ?? `Plan: ${props.mode}`}. Click for ${next}.`}
      onClick={() => void change()}
    >
      P
    </button>
  );
}

/**
 * Under the message box, always in view: the model that answers and how much
 * of its context the latest call used (marked from 90%, where the daemon
 * starts trimming).
 */
function SessionBar(props: { model: string; provider: string; context?: { used: number; budget: number } }) {
  const { model, provider, context } = props;
  const known = !!context && context.budget > 0;
  const share = known ? context.used / context.budget : 0;
  return (
    <div class={styles.controls}>
      <span class={styles.model} title={model ? `The model that answers: ${model}, on ${provider}` : undefined}>
        {provider && <span class={styles.provider}>{provider}</span>}
        {provider && model && <span aria-hidden="true"> | </span>}
        {model}
      </span>
      {known ? (
        <span
          class={share >= 0.9 ? styles.ctxWarn : styles.ctx}
          title={`The latest model call used ${context.used} of ${context.budget} context tokens${share >= 0.9 ? '; past 90% the daemon trims older context' : ''}`}
        >
          {share >= 0.9 && '⚠ '}ctx {tokens(context.used)} / {tokens(context.budget)}
        </span>
      ) : (
        <span class={styles.ctx} title="Context use shows once the session is loaded">
          ctx –
        </span>
      )}
    </div>
  );
}

/** A token count as a short figure: 950, 9.7k, 32.8k. */
function tokens(n: number): string {
  return n < 1000 ? String(n) : `${(n / 1000).toFixed(1)}k`;
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
