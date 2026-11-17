import { expect, signIn, test } from './fixtures';

// Phase 6 (adr/agents-concept.md A8): the Builder section — agents with
// run/export/import/approve, roles, skills, and the key-value memory.
//
// On a phone the Split layout shows the list XOR the open item, so list
// assertions always happen after navigating back to the bare list URL.

const esc = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

test('the Builder is reachable from the section navigation and its views work', async ({ page }) => {
  await signIn(page);
  await page.goto('/');
  await page.getByRole('navigation', { name: 'Sections' }).getByRole('link', { name: 'Builder', exact: true }).click();
  await expect(page).toHaveURL(/\/builder/);
  // On a desktop the landing pane says what the section is for — the page is
  // never bare. (On a phone the list itself is the landing; the pane opens with
  // the first item.)
  if (test.info().project.name === 'desktop') {
    await expect(page.getByRole('heading', { name: 'Agents', level: 2 })).toBeVisible();
  }
  const views = page.getByRole('navigation', { name: 'Builder views' });
  await expect(page.getByRole('navigation', { name: 'Agents' })).toBeVisible();
  for (const label of ['Roles', 'Skills', 'Data']) {
    await views.getByRole('link', { name: label, exact: true }).click();
    await expect(page.getByRole('navigation', { name: label === 'Data' ? 'Memory keys' : label })).toBeVisible();
  }
});

test('an agent is created, run, exported, deleted, imported staged and approved', async ({ page }) => {
  await signIn(page);
  const name = `e2e-builder-agent-${Date.now()}`;
  const rowRe = new RegExp(esc(name));
  await page.goto('/builder/agents');
  const list = page.getByRole('navigation', { name: 'Agents' });

  // Create: the app routes to the new agent's page.
  await list.getByRole('link', { name: 'New agent' }).click();
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page.getByLabel('Skills (comma-separated)').fill('');
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page).toHaveURL(/\/builder\/agents\/[^/]+$/);

  // Run: lands on the new conversation.
  await page.getByRole('button', { name: 'Run', exact: true }).click();
  await expect(page).toHaveURL(/\/conversations\/[0-9a-f-]+$/);

  // Export: a bundle tar named after the agent.
  await page.goto('/builder/agents');
  await list.getByRole('link', { name: rowRe }).click();
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByRole('button', { name: 'Export', exact: true }).click(),
  ]);
  expect(download.suggestedFilename()).toMatch(new RegExp(`^${esc(name)}\\.agent\\.tar$`));
  const bundle = await download.path();

  // Delete, then import the bundle back: it lands staged, not runnable.
  await page.getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
  await expect(list.getByRole('link', { name: rowRe })).toHaveCount(0);

  await page.locator('input[type="file"]').setInputFiles(bundle);
  await expect(list.getByRole('status')).toContainText(/staged/);
  const imported = list.getByRole('link', { name: rowRe });
  await expect(imported).toBeVisible();
  await imported.click();
  await expect(page.getByRole('button', { name: 'Approve', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Run', exact: true })).toBeDisabled();

  // Approve: active, runnable.
  await page.getByRole('button', { name: 'Approve', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Approve', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Run', exact: true })).toBeEnabled();
});

test('a role is created, edited and deleted', async ({ page }) => {
  await signIn(page);
  const name = `e2e-role-${Date.now()}`;
  const rowRe = new RegExp(esc(name));
  await page.goto('/builder/roles');
  const list = page.getByRole('navigation', { name: 'Roles' });

  await list.getByRole('link', { name: 'New role' }).click();
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page.getByLabel("Description (the role's persona)").fill('an e2e role');
  await page.getByLabel('Tool allowlist (comma-separated)').fill('shell');
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await page.goto('/builder/roles');
  const row = list.getByRole('link', { name: rowRe });
  await expect(row).toBeVisible();

  // Edit: widen the allowlist, save, and the row's badge shows it. (The
  // structural toggles like Delegates are stripped by the daemon for
  // agent-authored roles — R-ROLE.7 — so the allowlist is what round-trips.)
  await row.click();
  await page.getByLabel('Tool allowlist (comma-separated)').fill('shell, read');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await page.goto('/builder/roles');
  await expect(list.getByRole('link', { name: rowRe })).toContainText('2 tools');

  // Delete.
  await list.getByRole('link', { name: rowRe }).click();
  await page.getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
  await expect(list.getByRole('link', { name: rowRe })).toHaveCount(0);
});

test('a skill is created, edited and deleted', async ({ page }) => {
  await signIn(page);
  const name = `e2e-skill-${Date.now()}`;
  const rowRe = new RegExp(esc(name));
  await page.goto('/builder/skills');
  const list = page.getByRole('navigation', { name: 'Skills' });

  await list.getByRole('link', { name: 'New skill' }).click();
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page.getByLabel('Description', { exact: true }).fill('an e2e skill');
  await page.getByLabel('Body (Markdown)').fill('# E2E skill\nv1');
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  // Creating routes to the skill's page; back on the list its source is
  // agent-written.
  await page.goto('/builder/skills');
  const row = list.getByRole('link', { name: rowRe });
  await expect(row).toBeVisible();
  await expect(row).toContainText('agent');

  await row.click();
  await page.getByLabel('Body (Markdown)').fill('# E2E skill\nv2');
  await page.getByRole('button', { name: 'Save', exact: true }).click();

  await page.getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
  await expect(list.getByRole('link', { name: rowRe })).toHaveCount(0);
});

test('a memory key is created, edited and deleted', async ({ page }) => {
  await signIn(page);
  const key = `e2e/key-${Date.now()}`;
  const rowRe = new RegExp(esc(key));
  await page.goto('/builder/data');
  const list = page.getByRole('navigation', { name: 'Memory keys' });

  await list.getByRole('link', { name: 'New key' }).click();
  await expect(page).toHaveURL(/\/builder\/data\/new$/);
  await page.getByLabel('Key', { exact: true }).fill(key);
  await page.getByLabel('Value', { exact: true }).fill('hello');
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page).toHaveURL(/\/builder\/data\?key=/);

  // The new key shows up in the list.
  await page.goto('/builder/data');
  const row = list.getByRole('link', { name: rowRe });
  await expect(row).toBeVisible();

  await row.click();
  await page.getByLabel('Value', { exact: true }).fill('world');
  await page.getByRole('button', { name: 'Save', exact: true }).click();

  await page.getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
  await expect(page).toHaveURL(/\/builder\/data$/);
  await expect(list.getByRole('link', { name: rowRe })).toHaveCount(0);
});
