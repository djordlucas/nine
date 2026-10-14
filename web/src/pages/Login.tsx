import { useState } from 'preact/hooks';
import { signIn } from '../api/client';
import styles from './Login.module.css';

/** Exchanges the API token for a session cookie (adr/web-ui.md §3). */
export function Login() {
  const [token, setToken] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: Event) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      setError(await signIn(token));
    } catch {
      setError('The server did not answer.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <div class={styles.page}>
      <form class={`card ${styles.form}`} onSubmit={submit}>
        <div class={styles.brand} aria-hidden="true">
          9
        </div>
        <h1>Sign in to Nine</h1>
        <p class="muted">
          Enter the API token: <code>[api] auth_token</code> or <code>NINE_API_AUTH_TOKEN</code>.
        </p>
        <label class={styles.label} for="token">
          API token
        </label>
        <input
          id="token"
          class="input"
          type="password"
          autoComplete="current-password"
          value={token}
          onInput={(e) => setToken(e.currentTarget.value)}
          required
        />
        {error && (
          <p class="error" role="alert">
            {error}
          </p>
        )}
        <button class="button" type="submit" disabled={busy || token === ''}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </div>
  );
}
