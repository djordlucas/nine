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
  for (const label of ['Roles', 'Skills', 'Data', 'Tools', 'Plugins']) {
    await views.getByRole('link', { name: label, exact: true }).click();
    if (label === 'Plugins') {
      const plugins = page.getByRole('region', { name: 'Plugins' });
      await expect(plugins).toBeVisible();
      await expect(plugins).toContainText(/\d+ plugins?/);
    } else {
      await expect(page.getByRole('navigation', { name: label === 'Data' ? 'Memory keys' : label })).toBeVisible();
    }
  }
  // The bare Data view lands on an explanation card, not the New-key form.
  if (test.info().project.name === 'desktop') {
    await page.goto('/builder/data');
    await expect(page.getByRole('heading', { name: 'Data', level: 2 })).toBeVisible();
    await expect(page.getByRole('form', { name: 'New memory key' })).toHaveCount(0);
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
  await expect(list).toContainText(/\d+ roles?/);
  const row = list.getByRole('link', { name: rowRe });
  await expect(row).toBeVisible();

  // The list filters, and the form warns about allowlist names no tool has.
  await list.getByRole('searchbox', { name: 'Filter roles' }).fill(name.slice(-8));
  await expect(list.getByRole('link', { name: rowRe })).toBeVisible();
  await list.getByRole('searchbox', { name: 'Filter roles' }).fill('zzz-no-such-role');
  await expect(page.getByText('No role matches.')).toBeVisible();
  await list.getByRole('searchbox', { name: 'Filter roles' }).fill('');

  // Edit: widen the allowlist, save, and the row's badge shows it. (The
  // structural toggles like Delegates are stripped by the daemon for
  // agent-authored roles — R-ROLE.7 — so the allowlist is what round-trips.)
  await row.click();
  const tools = page.getByLabel('Tool allowlist (comma-separated)');
  await tools.fill('shell, not-a-real-tool');
  await expect(page.getByText('Unknown tools: not-a-real-tool')).toBeVisible();
  // Editing flags the form as unsaved until it is saved.
  await expect(page.getByText('Unsaved changes')).toBeVisible();
  await tools.fill('shell, read');
  await expect(page.getByText('Unknown tools: not-a-real-tool')).toHaveCount(0);
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('Unsaved changes')).toHaveCount(0);
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

  // The new key shows up in the list, with its size and age.
  await page.goto('/builder/data');
  await expect(list).toContainText(/\d+ keys?/);
  const row = list.getByRole('link', { name: rowRe });
  await expect(row).toBeVisible();
  await expect(row).toContainText('just now');

  await row.click();
  await page.getByLabel('Value', { exact: true }).fill('world');
  await page.getByRole('button', { name: 'Save', exact: true }).click();

  await page.getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
  await expect(page).toHaveURL(/\/builder\/data$/);
  await expect(list.getByRole('link', { name: rowRe })).toHaveCount(0);
});

test('the agent list filters and counts, skills attach as chips, and edits flag unsaved changes', async ({ page }) => {
  await signIn(page);
  // A skill to attach, created through the Skills view.
  const skillName = `e2e-chip-skill-${Date.now()}`;
  await page.goto('/builder/skills');
  await page.getByRole('navigation', { name: 'Skills' }).getByRole('link', { name: 'New skill' }).click();
  await page.getByLabel('Name', { exact: true }).fill(skillName);
  await page.getByLabel('Body (Markdown)').fill('# chip skill');
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page).toHaveURL(/\/builder\/skills\//);

  // New agent: the skill is a chip; toggling it selects it.
  const agentName = `e2e-chip-agent-${Date.now()}`;
  await page.goto('/builder/agents');
  const list = page.getByRole('navigation', { name: 'Agents' });
  await expect(list).toContainText(/\d+ agents?/);
  await list.getByRole('link', { name: 'New agent' }).click();
  const chip = page.getByRole('group', { name: 'Skills' }).getByRole('button', { name: skillName, exact: true });
  await expect(chip).toHaveAttribute('aria-pressed', 'false');
  await chip.click();
  await expect(chip).toHaveAttribute('aria-pressed', 'true');
  await page.getByLabel('Name', { exact: true }).fill(agentName);
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page).toHaveURL(/\/builder\/agents\/[^/]+$/);

  // Editing the loaded form flags unsaved changes until it is saved away.
  // (Wait for the load: the record line only shows once the agent is fetched,
  // and the load fills the fields, which would overwrite an early edit.)
  await expect(page.getByText(/^agent_[0-9a-f-]+ · v\d+ · /)).toBeVisible();
  await expect(page.getByLabel('Name', { exact: true })).toHaveValue(agentName);
  await page.getByLabel('Name', { exact: true }).fill(`${agentName}-x`);
  await expect(page.getByText('Unsaved changes')).toBeVisible();
  await page.reload();
  await expect(page.getByText('Unsaved changes')).toHaveCount(0);

  // The row shows the attached skill and its age; the filter narrows the list.
  await page.goto('/builder/agents');
  const row = list.getByRole('link', { name: new RegExp(esc(agentName)) });
  await expect(row).toContainText('1 skills');
  await expect(row).toContainText('just now');
  await list.getByRole('searchbox', { name: 'Filter agents' }).fill(agentName.slice(-8));
  await expect(list.getByRole('link', { name: new RegExp(esc(agentName)) })).toBeVisible();
  await list.getByRole('searchbox', { name: 'Filter agents' }).fill('zzz-no-such-agent');
  await expect(page.getByText('No agent matches.')).toBeVisible();
});
