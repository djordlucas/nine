import createClient, { type Middleware } from 'openapi-fetch';
import { signal } from '@preact/signals';
import type { components, paths } from './schema';

export type Schemas = components['schemas'];

/**
 * Where the browser stands with the API. `checking` until the first
 * GET /auth/session answers; `signed-out` when the server requires a token
 * this browser has not exchanged for a session.
 */
export type AuthState = 'checking' | 'signed-in' | 'signed-out' | 'unreachable';

export const auth = signal<AuthState>('checking');
/** The server requires a token: the UI offers sign-out only then. */
export const authRequired = signal(false);

// The CSRF token of this browser's cookie session. Held in memory only: a
// reload gets it again from GET /auth/session, which a cross-site page cannot
// read.
let csrf: string | undefined;

const csrfAndExpiry: Middleware = {
  onRequest({ request }) {
    if (csrf && request.method !== 'GET' && request.method !== 'HEAD') {
      request.headers.set('X-Nine-CSRF', csrf);
    }
    return request;
  },
  onResponse({ response, request }) {
    // A 401 outside the session endpoint means the session ended: expired,
    // signed out elsewhere, or the API server restarted.
    if (response.status === 401 && !new URL(request.url).pathname.endsWith('/auth/session')) {
      csrf = undefined;
      auth.value = 'signed-out';
    }
    return response;
  },
};

export const api = createClient<paths>({
  // Absolute, because Request needs one outside a document (in tests).
  baseUrl: `${location.origin}/api/v1`,
  credentials: 'same-origin',
});
api.use(csrfAndExpiry);

/** Message for an API error body, or a fallback. */
export function errorMessage(error: unknown, fallback = 'Request failed'): string {
  const e = error as Schemas['ErrorResponse'] | undefined;
  return e?.error?.message ?? fallback;
}

function apply(s: Schemas['AuthSessionResponse']) {
  csrf = s.csrf_token;
  authRequired.value = s.auth_required;
  auth.value = s.authenticated ? 'signed-in' : 'signed-out';
}

/** Asks the server whether this browser is signed in. */
export async function checkSession(): Promise<void> {
  try {
    const { data } = await api.GET('/auth/session');
    if (!data) {
      auth.value = 'unreachable';
      return;
    }
    apply(data);
  } catch {
    auth.value = 'unreachable';
  }
}

/** Exchanges the API token for a session cookie. Returns an error message, or null. */
export async function signIn(token: string): Promise<string | null> {
  const { data, error, response } = await api.POST('/auth/session', { body: { token } });
  if (data) {
    apply(data);
    return null;
  }
  if (response.status === 401) return 'That token is not the one this server is configured with.';
  return errorMessage(error);
}

export async function signOut(): Promise<void> {
  await api.DELETE('/auth/session');
  csrf = undefined;
  auth.value = 'signed-out';
}
