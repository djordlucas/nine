import { expect, signIn, test, token } from './fixtures';

// The Data section (adr/web-ui.md §10): workspace files and the database.

const bearer = { Authorization: `Bearer ${token}` };

test('a workspace file is edited, uploaded beside and deleted; .nine stays private', async ({ page, request }, info) => {
  const dir = `e2e-data/${info.project.name}-${Date.now()}`;
  const put = await request.put(`/api/v1/workspace/file?path=${encodeURIComponent(`${dir}/note.md`)}`, {
    headers: { ...bearer, 'Content-Type': 'application/octet-stream' },
    data: Buffer.from('first line\n'),
  });
  expect(put.ok()).toBe(true);

  await signIn(page);
  await page.goto('/data/files');
  const files = page.getByRole('navigation', { name: 'Files' });
  await expect(files.getByRole('link', { name: /e2e-data/ })).toBeVisible();
  await expect(files.getByRole('link', { name: /\.nine/ })).toHaveCount(0);

  await page.goto(`/data/files?dir=${encodeURIComponent(dir)}`);
  await files.getByRole('link', { name: /note\.md/ }).click();
  const editor = page.getByRole('textbox', { name: 'Contents of note.md' });
  await expect(editor).toHaveValue('first line\n');
  await editor.fill('second version\n');
  await page.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByText('Saved. The previous version is in the trash.')).toBeVisible();
  const read = await request.get(`/api/v1/workspace/file?path=${encodeURIComponent(`${dir}/note.md`)}`, { headers: bearer });
  expect(await read.text()).toBe('second version\n');

  // A write against the version the page read is refused once someone else has changed the file.
  await request.put(`/api/v1/workspace/file?path=${encodeURIComponent(`${dir}/note.md`)}`, {
    headers: { ...bearer, 'Content-Type': 'application/octet-stream' },
    data: Buffer.from('changed elsewhere\n'),
  });
  await editor.fill('mine\n');
  await page.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByRole('region', { name: 'File' }).getByRole('alert')).toContainText('changed since it was read');

  await page.goto(`/data/files?dir=${encodeURIComponent(dir)}`);
  await files.locator('input[type=file]').setInputFiles({ name: 'data.bin', mimeType: 'application/octet-stream', buffer: Buffer.from([0, 1, 2, 255]) });
  await files.getByRole('link', { name: /data\.bin/ }).click();
  await expect(page.getByText('Not a text file.', { exact: false })).toBeVisible();

  page.once('dialog', (d) => void d.accept());
  await page.getByRole('region', { name: 'File' }).getByRole('button', { name: 'Delete' }).click();
  await expect(page).toHaveURL(new RegExp(`dir=${encodeURIComponent(dir).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`));
  await expect(files.getByRole('link', { name: /data\.bin/ })).toHaveCount(0);
});

test('a table is created, a row added, changed and deleted', async ({ page }, info) => {
  const table = `e2e_${info.project.name}_${Date.now()}`;
  await signIn(page);
  await page.goto('/data/sql');
  await page.getByRole('textbox', { name: 'Statement' }).fill(`CREATE TABLE ${table} (name TEXT NOT NULL, n INTEGER)`);
  await page.getByRole('button', { name: 'Run' }).click();
  const sql = page.getByRole('region', { name: 'Run a statement' });
  await expect(sql.getByRole('status')).toContainText('0 rows changed');

  await page.goto('/data/database');
  await page.getByRole('navigation', { name: 'Tables' }).getByRole('link', { name: new RegExp(table) }).click();
  const pane = page.getByRole('region', { name: 'Table' });
  await pane.getByRole('button', { name: 'Add row' }).click();
  const form = pane.getByRole('form', { name: 'New row' });
  await form.getByLabel(/^name/).fill('alpha');
  await form.getByLabel(/^n\b/).fill('7');
  await form.getByRole('button', { name: 'Insert' }).click();
  const grid = pane.getByRole('table', { name: `Rows of ${table}` });
  await expect(grid.getByRole('cell', { name: 'alpha' })).toBeVisible();

  await grid.getByRole('button', { name: 'Edit' }).click();
  const row = pane.getByRole('form', { name: 'Row 1' });
  await row.getByLabel(/^name/).fill('beta');
  await row.getByRole('button', { name: 'Save' }).click();
  await expect(grid.getByRole('cell', { name: 'beta' })).toBeVisible();

  await page.goto('/data/sql');
  await page.getByRole('textbox', { name: 'Statement' }).fill(`SELECT typeof(n) AS t FROM ${table}`);
  await page.getByRole('button', { name: 'Run' }).click();
  await expect(page.getByRole('table', { name: 'Result' }).getByRole('cell', { name: 'integer' })).toBeVisible();

  await page.goto(`/data/database/${table}`);
  await page.getByRole('table', { name: `Rows of ${table}` }).getByRole('button', { name: 'Edit' }).click();
  page.once('dialog', (d) => void d.accept());
  await page.getByRole('button', { name: 'Delete row' }).click();
  await expect(page.getByText('No rows')).toBeVisible();

  await page.goto('/data/sql');
  await page.getByRole('textbox', { name: 'Statement' }).fill(`ATTACH DATABASE ':memory:' AS o`);
  await page.getByRole('button', { name: 'Run' }).click();
  await expect(sql.getByRole('alert')).toContainText('attach');
});
