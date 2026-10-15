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

  const download = page.waitForEvent('download');
  await page.getByRole('link', { name: 'Download the store (.db)' }).click();
  const file = await download;
  expect(file.suggestedFilename()).toMatch(/^nine-\d{8}-\d{6}\.db$/);
  const bytes = await (await file.createReadStream()).toArray();
  expect(Buffer.concat(bytes).subarray(0, 15).toString()).toBe('SQLite format 3');
});

test('a session is listed, opened and deleted', async ({ page, request }) => {
  const res = await request.post('/api/v1/conversations', { headers: bearer, data: {} });
  expect(res.ok()).toBe(true);
  const id = (await res.json()).id as string;
  // The roster holds a session once it has a turn.
  const turn = await request.post(`/api/v1/conversations/${id}/messages`, { headers: bearer, data: { text: 'hello' } });
  expect(turn.ok()).toBe(true);

  await signIn(page);
  await page.goto('/system/sessions');
  await page.getByRole('searchbox', { name: 'Filter sessions' }).fill(id);
  const row = page.getByRole('navigation', { name: 'Sessions' }).locator(`a[href="/system/sessions/${id}"]`);
  await row.click();
  const session = page.getByRole('region', { name: 'Session' });
  await expect(session.getByText(id, { exact: true })).toBeVisible();

  page.once('dialog', (d) => void d.accept());
  await session.getByRole('button', { name: 'Delete' }).click();
  await expect(page).toHaveURL(/\/system\/sessions$/);
  await expect(row).toHaveCount(0);
});

test('reflections say when there are none', async ({ page }) => {
  await signIn(page);
  await page.goto('/system/reflections');
  await page.getByRole('textbox', { name: 'Reflecting session' }).fill('no-such-session');
  await page.getByRole('button', { name: 'Show' }).click();
  await expect(page.getByText('No reflections for no-such-session yet.')).toBeVisible();
});

test('a docs topic opens, and a link in it opens another in place', async ({ page }) => {
  await signIn(page);
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
