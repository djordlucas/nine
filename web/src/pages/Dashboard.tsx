import { useEffect, useState } from 'preact/hooks';
import { api, errorMessage, type Schemas } from '../api/client';
import styles from './Dashboard.module.css';

/** Home (adr/web-ui.md §5). Tiles join it as their sections are built. */
export function Dashboard() {
  const [status, setStatus] = useState<Schemas['StatusResponse'] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.GET('/status').then(({ data, error }) => {
      if (data) setStatus(data);
      else setError(errorMessage(error, 'Could not load the daemon status.'));
    });
  }, []);

  return (
    <div class={styles.grid}>
      <section class="card" aria-labelledby="status-h">
        <h2 id="status-h" class={styles.tileTitle}>
          Daemon
        </h2>
        {error && <p class="error">{error}</p>}
        {!error && !status && <p class="muted">Loading…</p>}
        {status && (
          <dl class={styles.facts}>
            <dt>Status</dt>
            <dd>{status.status ?? '—'}</dd>
            <dt>Version</dt>
            <dd>{status.version ?? '—'}</dd>
            <dt>Uptime</dt>
            <dd>{status.uptime ?? '—'}</dd>
            <dt>Active sessions</dt>
            <dd>{status.active_sessions ?? 0}</dd>
            <dt>Plugins</dt>
            <dd>{status.plugins?.length ?? 0}</dd>
            <dt>Tools</dt>
            <dd>{status.tools?.length ?? 0}</dd>
          </dl>
        )}
      </section>
    </div>
  );
}
