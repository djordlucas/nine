import { useEffect, useRef, useState } from 'preact/hooks';
import { subscribe, typeMatches, type JournalEvent } from './api/events';

/** How long a burst of events waits before the reload it causes. */
export const SETTLE_MS = 400;

export interface Live<T> {
  data: T | null;
  error: string | null;
}

/**
 * Loads data and loads it again when the event stream reports a change it
 * depends on: an event whose type matches one of types (exact, or a prefix
 * ending in *), or a reset. A burst of events causes one reload.
 *
 * load returns the data, or throws an Error whose message is shown. A change
 * of reloadKey loads again too.
 */
export function useLive<T>(load: () => Promise<T>, types: readonly string[], reloadKey?: unknown): Live<T> {
  const [state, setState] = useState<Live<T>>({ data: null, error: null });
  const loadRef = useRef(load);
  loadRef.current = load;
  const key = types.join(',');

  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const run = () => {
      loadRef.current().then(
        (data) => alive && setState({ data, error: null }),
        (err: unknown) =>
          alive && setState((s) => ({ data: s.data, error: err instanceof Error ? err.message : String(err) })),
      );
    };
    const soon = () => {
      clearTimeout(timer);
      timer = setTimeout(run, SETTLE_MS);
    };
    run();
    const off = subscribe({
      event: (e: JournalEvent) => {
        if (typeMatches(e.type, types)) soon();
      },
      reset: soon,
    });
    return () => {
      alive = false;
      clearTimeout(timer);
      off();
    };
    // types is compared by value through key.
  }, [key, reloadKey]);

  return state;
}

/** The events of the stream that match types, newest first, at most n. */
export function useRecent(types: readonly string[], n: number, initial: () => readonly JournalEvent[]): JournalEvent[] {
  const [events, setEvents] = useState<JournalEvent[]>(() =>
    initial()
      .filter((e) => typeMatches(e.type, types))
      .slice(-n)
      .reverse(),
  );
  const key = types.join(',');
  useEffect(
    () =>
      subscribe({
        event: (e) => {
          if (typeMatches(e.type, types)) setEvents((prev) => [e, ...prev].slice(0, n));
        },
        reset: () => setEvents([]),
      }),
    [key, n],
  );
  return events;
}
