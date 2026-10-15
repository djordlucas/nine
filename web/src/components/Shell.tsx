import type { ComponentChildren } from 'preact';
import { useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { sections, sectionFor } from '../sections';
import { authRequired, signOut } from '../api/client';
import { streamState } from '../api/events';
import { cycleTheme, theme } from '../theme';
import { attentionEvents, loadAttention } from '../inbox';
import { useLive } from '../live';
import { Icon } from './Icon';
import styles from './Shell.module.css';

/**
 * The frame around every section: a rail on desktop; a header and a bottom
 * tab bar under 768px, with a More sheet for the sections the bar has no room
 * for.
 */
export function Shell({ children }: { children: ComponentChildren }) {
  const { path } = useLocation();
  const current = sectionFor(path);
  const [more, setMore] = useState(false);
  const tabs = sections.filter((s) => s.tab);
  const rest = sections.filter((s) => !s.tab);
  const inMore = !current.tab;
  const waiting = useWaiting();

  return (
    <div class={styles.shell}>
      <nav class={styles.rail} aria-label="Sections">
        <a class={styles.brand} href="/" aria-label="Nine">
          9
        </a>
        {sections.map((s) => (
          <a
            key={s.path}
            href={s.path}
            class={styles.railLink}
            aria-current={s === current ? 'page' : undefined}
            title={s.label}
          >
            <Icon name={s.icon} />
            <span>{s.label}</span>
            {s.path === '/inbox' && <Badge n={waiting} />}
          </a>
        ))}
        <div class={styles.railFoot}>
          <Controls />
        </div>
      </nav>

      <div class={styles.main}>
        <header class={styles.header}>
          <h1>{current.label}</h1>
          <LiveState />
        </header>
        <main class={styles.content}>{children}</main>
      </div>

      <nav class={styles.tabs} aria-label="Sections">
        {tabs.map((s) => (
          <a
            key={s.path}
            href={s.path}
            class={styles.tab}
            aria-current={s === current ? 'page' : undefined}
            onClick={() => setMore(false)}
          >
            <Icon name={s.icon} />
            <span>{s.label}</span>
            {s.path === '/inbox' && <Badge n={waiting} />}
          </a>
        ))}
        <button
          type="button"
          class={styles.tab}
          aria-current={inMore ? 'page' : undefined}
          aria-expanded={more}
          aria-controls="more-sheet"
          onClick={() => setMore(!more)}
        >
          <Icon name="more" />
          <span>More</span>
        </button>
      </nav>

      {more && (
        <div class={styles.scrim} onClick={() => setMore(false)}>
          <div
            id="more-sheet"
            class={styles.sheet}
            role="dialog"
            aria-label="More sections"
            onClick={(e) => e.stopPropagation()}
          >
            {rest.map((s) => (
              <a key={s.path} href={s.path} class={styles.sheetLink} onClick={() => setMore(false)}>
                <Icon name={s.icon} />
                <span>{s.label}</span>
              </a>
            ))}
            <div class={styles.sheetFoot}>
              <Controls />
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

/** How many approvals and questions wait for the operator; 0 until known. */
function useWaiting(): number {
  const { data } = useLive(loadAttention, attentionEvents);
  return data ? data.requests.length + data.questions.length : 0;
}

/** The count of what waits in the Inbox, on its link (adr/web-ui.md §5). */
function Badge({ n }: { n: number }) {
  if (n <= 0) return null;
  return (
    <span class={styles.badge} aria-label={`${n} waiting`}>
      {n}
    </span>
  );
}

/** Whether the event stream is up: what the live views show depends on it. */
function LiveState() {
  const state = streamState.value;
  const label = { live: 'Live', connecting: 'Connecting…', offline: 'Offline' }[state];
  return (
    <span class={styles.live} data-state={state} role="status" title="The event stream that keeps this page current">
      {label}
    </span>
  );
}

function Controls() {
  const label = { system: 'Theme: system', light: 'Theme: light', dark: 'Theme: dark' }[theme.value];
  return (
    <>
      <button type="button" class={styles.control} onClick={cycleTheme}>
        {label}
      </button>
      {authRequired.value && (
        <button type="button" class={styles.control} onClick={() => void signOut()}>
          Sign out
        </button>
      )}
    </>
  );
}
