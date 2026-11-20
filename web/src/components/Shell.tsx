import type { ComponentChildren } from 'preact';
import { useLocation } from 'preact-iso';
import { sections, sectionFor } from '../sections';
import { api, authRequired, get, signOut } from '../api/client';
import { streamState } from '../api/events';
import { cycleTheme, theme } from '../theme';
import { attentionEvents, loadAttention } from '../inbox';
import { headerSlot } from '../headerSlot';
import { useLive } from '../live';
import { Icon } from './Icon';
import styles from './Shell.module.css';

/**
 * The frame around every section: a rail on desktop; a header and a bottom
 * tab bar holding every section under 768px.
 */
export function Shell({ children }: { children: ComponentChildren }) {
  const { path } = useLocation();
  const current = sectionFor(path);
  const foot = sections.filter((s) => s.foot);
  const waiting = useWaiting();
  const instance = useInstanceName();

  return (
    <div class={styles.shell}>
      <nav class={styles.rail} aria-label="Sections">
        <a class={styles.brand} href="/">
          Nine
          {instance && <span class={styles.instance} title="This instance's name">{instance}</span>}
        </a>
        {sections.filter((s) => !s.foot).map((s) => (
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
          {foot.map((s) => (
            <a
              key={s.path}
              href={s.path}
              class={styles.railLink}
              aria-current={s === current ? 'page' : undefined}
              title={s.label}
            >
              <Icon name={s.icon} />
              <span>{s.label}</span>
            </a>
          ))}
          <Controls />
          <LiveState />
        </div>
      </nav>

      <div class={styles.main}>
        <header class={styles.header}>
          <h1>{current.label}</h1>
          {headerSlot.value && <div class={styles.headerSlot}>{headerSlot.value}</div>}
          {/* On a phone the rail is gone, so the live state and the controls
             live in this top bar instead. */}
          <div class={styles.headerFoot}>
            <LiveState />
            <Controls />
          </div>
        </header>
        <main class={styles.content}>{children}</main>
      </div>

      <nav class={styles.tabs} aria-label="Sections">
        {sections.map((s) => (
          <a
            key={s.path}
            href={s.path}
            class={styles.tab}
            aria-current={s === current ? 'page' : undefined}
          >
            <Icon name={s.icon} size={20} />
            <span class={styles.tabLabel}>{s.label}</span>
            {s.path === '/inbox' && <Badge n={waiting} />}
          </a>
        ))}
      </nav>
    </div>
  );
}

/**
 * This instance's display name, from the daemon's status; empty until known,
 * and while it is still the placeholder "nine" the word mark already says.
 */
function useInstanceName(): string {
  const { data } = useLive(async () => (await get(api.GET('/status'), 'the daemon status')).instance_name ?? '', []);
  return data && data.toLowerCase() !== 'nine' ? data : '';
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
      <button type="button" class={styles.control} onClick={cycleTheme} aria-label={label} title={label}>
        <Icon name="theme" size={16} />
        <span class={styles.controlText}>{label}</span>
      </button>
      {authRequired.value && (
        <button type="button" class={styles.control} onClick={() => void signOut()} aria-label="Sign out" title="Sign out">
          <Icon name="power" size={16} />
          <span class={styles.controlText}>Sign out</span>
        </button>
      )}
    </>
  );
}
