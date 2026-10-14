import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { BUFFER, recentEvents, streamState, subscribe, type JournalEvent } from '../api/events';
import { eventKind, omittedBytes, shortId, summarize, timeOf, type EventKind } from '../journal';
import styles from './Journal.module.css';

/** How many rows render at once; more on request. */
export const PAGE = 200;

const kinds: { value: EventKind | ''; label: string }[] = [
  { value: '', label: 'Every event' },
  { value: 'turn', label: 'Turns' },
  { value: 'llm', label: 'Model calls' },
  { value: 'tool', label: 'Tool calls' },
  { value: 'agent', label: 'Sub-agents' },
  { value: 'process', label: 'Processes' },
  { value: 'attention', label: 'Needs you' },
  { value: 'state', label: 'State changes' },
];

/**
 * The journal, live (adr/web-ui.md §5): the events the stream has kept, newest
 * first, then each new one as it arrives.
 */
export function Journal() {
  const [events, setEvents] = useState<JournalEvent[]>(() => [...recentEvents()].reverse());
  const [held, setHeld] = useState<JournalEvent[]>([]);
  const [paused, setPaused] = useState(false);
  // The listener reads this rather than paused: an effect re-subscribing on a
  // change of paused runs after the next frame, and an event arriving before
  // it would land in the list the operator just paused.
  const pausedRef = useRef(false);
  const [missed, setMissed] = useState(false);
  const [kind, setKind] = useState<EventKind | ''>('');
  const [agent, setAgent] = useState('');
  const [limit, setLimit] = useState(PAGE);
  const [open, setOpen] = useState<number | null>(null);

  useEffect(
    () =>
      subscribe({
        event: (e) => {
          if (pausedRef.current) setHeld((h) => [e, ...h]);
          else setEvents((prev) => [e, ...prev].slice(0, BUFFER));
        },
        reset: () => {
          setEvents([]);
          setHeld([]);
          setMissed(true);
        },
      }),
    [],
  );

  const pause = () => {
    pausedRef.current = true;
    setPaused(true);
  };
  const resume = () => {
    pausedRef.current = false;
    setEvents((prev) => [...held, ...prev].slice(0, BUFFER));
    setHeld([]);
    setPaused(false);
  };

  const shown = useMemo(() => {
    const a = agent.trim();
    return events.filter((e) => (!kind || eventKind(e.type) === kind) && (!a || e.agent_id.startsWith(a)));
  }, [events, kind, agent]);

  return (
    <div class={styles.page}>
      <div class={`card ${styles.toolbar}`} role="search">
        <label class={styles.field}>
          <span>Show</span>
          <select class="input" value={kind} onChange={(e) => setKind((e.target as HTMLSelectElement).value as EventKind | '')}>
            {kinds.map((k) => (
              <option key={k.value} value={k.value}>
                {k.label}
              </option>
            ))}
          </select>
        </label>
        <label class={styles.field}>
          <span>Session or process</span>
          <input
            class="input"
            type="search"
            placeholder="id or its start"
            value={agent}
            onInput={(e) => setAgent((e.target as HTMLInputElement).value)}
          />
        </label>
        <button
          type="button"
          class={`button ${styles.pause}`}
          aria-pressed={paused}
          onClick={() => (paused ? resume() : pause())}
        >
          {paused ? (held.length ? `Resume (${held.length} new)` : 'Resume') : 'Pause'}
        </button>
      </div>

      <p class={styles.status} aria-live="polite">
        <span class={`${styles.dot} ${styles[streamState.value]}`} aria-hidden="true" />
        {streamState.value === 'live' ? 'Live' : streamState.value === 'connecting' ? 'Connecting…' : 'Offline, retrying'}
        <span class="muted">
          {' '}
          · {shown.length} of {events.length} kept events
        </span>
      </p>
      {missed && (
        <p class={`card ${styles.notice}`} role="status">
          Some events were missed while the stream was away; the list starts again from here.
        </p>
      )}

      {shown.length === 0 ? (
        <p class="card muted">{events.length ? 'No kept event matches.' : 'No events yet. They appear here as they happen.'}</p>
      ) : (
        <ol class={styles.list} aria-label="Journal events">
          {shown.slice(0, limit).map((e) => (
            <Row key={e.seq} e={e} open={open === e.seq} toggle={() => setOpen(open === e.seq ? null : e.seq)} onAgent={setAgent} />
          ))}
        </ol>
      )}
      {shown.length > limit && (
        <button type="button" class={`button ${styles.moreButton}`} onClick={() => setLimit(limit + PAGE)}>
          Show {Math.min(PAGE, shown.length - limit)} more
        </button>
      )}
    </div>
  );
}

function Row({ e, open, toggle, onAgent }: { e: JournalEvent; open: boolean; toggle: () => void; onAgent: (a: string) => void }) {
  const omitted = omittedBytes(e);
  return (
    <li class={styles.row}>
      <button type="button" class={styles.rowHead} aria-expanded={open} onClick={toggle}>
        <span class={styles.time}>{timeOf(e)}</span>
        <span class={`${styles.type} ${styles[eventKind(e.type)]}`}>{e.type}</span>
        <span class={styles.who}>{shortId(e.agent_id)}</span>
        <span class={styles.summary}>{summarize(e)}</span>
      </button>
      {open && (
        <div class={styles.detail}>
          <dl class={styles.facts}>
            <dt>Seq</dt>
            <dd>{e.seq}</dd>
            <dt>Agent</dt>
            <dd>
              <button type="button" class={styles.link} onClick={() => onAgent(e.agent_id)} title="Show only this one">
                {e.agent_id}
              </button>
            </dd>
            {e.turn > 0 && (
              <>
                <dt>Turn</dt>
                <dd>{e.turn}</dd>
              </>
            )}
            <dt>Time</dt>
            <dd>{new Date(e.timestamp).toLocaleString()}</dd>
          </dl>
          {omitted !== null ? (
            <p class="muted">
              The payload ({Math.round(omitted / 1024)} KB) is too large for the stream.{' '}
              {e.turn > 0 && (
                <a href={`/api/v1/conversations/${encodeURIComponent(e.agent_id)}/trace?turn=${e.turn}`} target="_blank" rel="noopener">
                  Open turn {e.turn}'s trace
                </a>
              )}
            </p>
          ) : (
            e.payload !== undefined && <pre class={styles.payload}>{JSON.stringify(e.payload, null, 2)}</pre>
          )}
        </div>
      )}
    </li>
  );
}
