import { beforeEach, describe, expect, it, vi } from 'vitest';

// openapi-fetch captures fetch when the client is created, so the stub is
// installed before the module loads.
const calls: Request[] = [];
let respond: (req: Request) => Response;

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

vi.stubGlobal('fetch', async (input: Request) => {
  calls.push(input.clone());
  return respond(input);
});

const client = await import('./client');

beforeEach(() => {
  calls.length = 0;
});

describe('session client', () => {
  it('signs in and sends the CSRF token on writes only', async () => {
    respond = () => json(200, { auth_required: true, authenticated: true, csrf_token: 'tok' });
    expect(await client.signIn('secret')).toBeNull();
    expect(client.auth.value).toBe('signed-in');
    expect(client.authRequired.value).toBe(true);

    respond = () => json(200, {});
    await client.api.GET('/docs');
    await client.api.POST('/plugins/reload');
    expect(calls[1].headers.get('X-Nine-CSRF')).toBeNull();
    expect(calls[2].headers.get('X-Nine-CSRF')).toBe('tok');
  });

  it('reports a wrong token without signing in', async () => {
    respond = () => json(401, { error: { code: 'unauthorized', message: 'invalid token' } });
    client.auth.value = 'signed-out';
    expect(await client.signIn('guess')).toMatch(/not the one/);
    expect(client.auth.value).toBe('signed-out');
  });

  it('returns to sign-in when a request comes back 401', async () => {
    client.auth.value = 'signed-in';
    respond = () => json(401, { error: { code: 'unauthorized', message: 'missing authorization header' } });
    await client.api.GET('/docs');
    expect(client.auth.value).toBe('signed-out');
  });

  it('treats a server with no token as signed in', async () => {
    respond = () => json(200, { auth_required: false, authenticated: true });
    await client.checkSession();
    expect(client.auth.value).toBe('signed-in');
    expect(client.authRequired.value).toBe(false);
  });

  it('reports an unreachable server', async () => {
    respond = () => {
      throw new TypeError('network');
    };
    await client.checkSession();
    expect(client.auth.value).toBe('unreachable');
  });
});
