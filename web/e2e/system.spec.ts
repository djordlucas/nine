import { expect, signIn, test, token } from './fixtures';

// Phase 6 (adr/web-ui.md §9): the System section.

const bearer = { Authorization: `Bearer ${token}` };

test('the overview shows the daemon, a backup download and the CLI-only operations', async ({ page }) => {
  await signIn(page);
  await page.goto('/system');
  for (const name of ['Daemon', 'Backup', 'In the CLI only']) {
    await expect(page.getByRole('heading', { name, level: 2 })).toBeVisible();
  }
  await expect(page.getByText('nine restore <snapshot>')).toBeVisible();

  // The daemon card carries what /status reports: model and provider, and
  // links to the sessions, tools and store.
  const daemon = page.getByRole('region', { name: 'Daemon' });
  await expect(daemon.getByText('Model')).toBeVisible();
  await expect(daemon).toContainText('stub');
  await expect(daemon.getByText('Tools')).toBeVisible();
  await expect(daemon.getByRole('link', { name: /\d+/ }).first()).toBeVisible();

  const download = page.waitForEvent('download');
  await page.getByRole('link', { name: 'Download the store (.tar.gz)' }).click();
  const file = await download;
  expect(file.suggestedFilename()).toMatch(/^nine-\d{8}-\d{6}\.tar\.gz$/);
  // The archive holds the registry database (nine.db) and every agents/<id>.db,
  // each a SQLite file (adr/agents-concept.md A4, Q33).
  const zlib = await import('node:zlib');
  const bytes = await (await file.createReadStream()).toArray();
  const tar = zlib.gunzipSync(Buffer.concat(bytes));
  // The first entry is the registry database, a SQLite file.
  expect(tar.subarray(0, 100).toString()).toContain('nine.db');
  expect(tar.indexOf('SQLite format 3')).toBeGreaterThanOrEqual(0);
});

test('a session is listed, opened and deleted', async ({ page, request }) => {
  // The e2e origin is plain HTTP, so copyText takes the execCommand path;
  // capture what it selected.
  await page.addInitScript(() => {
    const orig = Document.prototype.execCommand;
    (window as unknown as { __copied: string }).__copied = '';
    Document.prototype.execCommand = function (cmd: string, ...args: unknown[]) {
      if (cmd === 'copy') (window as unknown as { __copied: string }).__copied = window.getSelection()?.toString() ?? '';
      return orig.call(this, cmd, ...(args as []));
    };
  });
  const res = await request.post('/api/v1/conversations', { headers: bearer, data: {} });
  expect(res.ok()).toBe(true);
  const id = (await res.json()).id as string;
  // The roster holds a session once it has a turn.
  const turn = await request.post(`/api/v1/conversations/${id}/messages`, { headers: bearer, data: { text: 'hello' } });
  expect(turn.ok()).toBe(true);

  await signIn(page);
  await page.goto('/system/sessions');
  const list = page.getByRole('navigation', { name: 'Sessions' });
  await expect(list).toContainText(/\d+ sessions?/);
  await page.getByRole('searchbox', { name: 'Filter sessions' }).fill(id);
  const row = list.locator(`a[href="/system/sessions/${id}"]`);
  await row.click();
  const session = page.getByRole('region', { name: 'Session' });
  await expect(session.getByText(id, { exact: true })).toBeVisible();

  // The id copies, and the conversation opens from a primary button.
  await session.getByRole('button', { name: 'Copy id' }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as { __copied: string }).__copied)).toBe(id);
  await expect(session.getByRole('link', { name: 'Open the conversation' })).toBeVisible();

  await session.getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click();
  await expect(page).toHaveURL(/\/system\/sessions$/);
  await expect(row).toHaveCount(0);
  // Back on the bare list, a desktop lands on the explanation card.
  if (test.info().project.name === 'desktop') {
    await expect(page.getByRole('heading', { name: 'Sessions', level: 2 })).toBeVisible();
  }
});

test('reflections say when there are none', async ({ page }) => {
  await signIn(page);
  await page.goto('/system/reflections');
  // An input with a list is a combobox in the a11y tree.
  const field = page.getByRole('combobox', { name: 'Reflecting session' });
  await expect(field).toHaveAttribute('list', 'reflection-sessions');
  await field.fill('no-such-session');
  await page.getByRole('button', { name: 'Show' }).click();
  await expect(page.getByText('No reflections for no-such-session yet.')).toBeVisible();
});

test('a docs topic opens, and a link in it opens another in place', async ({ page }) => {
  await signIn(page);
  await page.goto('/system/docs');
  await expect(page.getByRole('navigation', { name: 'Docs topics' })).toContainText(/\d+ topics?/);
  await page.goto('/system/docs/processes');
  const topic = page.getByRole('region', { name: 'Topic' });
  await expect(topic.getByRole('heading').first()).toBeVisible();
  const link = topic.locator('a[href^="/system/docs/"], a[href^="/system/spec/"]').first();
  const href = await link.getAttribute('href');
  expect(href).toMatch(/^\/system\/(docs|spec)\//);
  await link.click();
  await expect(page).toHaveURL(new RegExp(href!.split('#')[0].replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
  await expect(page.getByRole('region', { name: 'Topic' }).getByRole('heading').first()).toBeVisible();
});

test('the spec lists its topics', async ({ page }) => {
  await signIn(page);
  await page.goto('/system/spec');
  await expect(page.getByRole('navigation', { name: 'Spec topics' }).getByRole('link', { name: 'conformance' })).toBeVisible();
});

test('the config view shows the TOML and copies it', async ({ page }) => {
  // Plain-HTTP e2e origin: capture the execCommand('copy') fallback.
  await page.addInitScript(() => {
    const orig = Document.prototype.execCommand;
    (window as unknown as { __copied: string }).__copied = '';
    Document.prototype.execCommand = function (cmd: string, ...args: unknown[]) {
      if (cmd === 'copy') (window as unknown as { __copied: string }).__copied = window.getSelection()?.toString() ?? '';
      return orig.call(this, cmd, ...(args as []));
    };
  });
  await signIn(page);
  await page.goto('/system/config');
  await expect(page.getByRole('heading', { name: 'Configuration', level: 2 })).toBeVisible();
  await page.getByRole('button', { name: 'Copy the TOML' }).click();
  const copied = await page.evaluate(() => (window as unknown as { __copied: string }).__copied);
  expect(copied.length).toBeGreaterThan(20);
  expect(copied).toContain('=');
});
