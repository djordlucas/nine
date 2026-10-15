import { expect, signIn, test } from './fixtures';

// Phase 5 (adr/web-ui.md §9): tools, plugins, skills and the Inbox.

test('the tool roster groups tools, filters them, and calls one', async ({ page }) => {
  await signIn(page);
  await page.goto('/tools');
  const list = page.getByRole('navigation', { name: 'Tools', exact: true });
  await expect(list.getByRole('heading', { name: /Built in/ })).toBeVisible();

  await page.getByRole('searchbox', { name: 'Filter tools' }).fill('time');
  await list.getByRole('link', { name: /^time\b/ }).first().click();
  await expect(page.getByRole('heading', { name: 'time', level: 2 })).toBeVisible();
  await expect(page.getByText('Capabilities')).toBeVisible();

  await page.getByRole('button', { name: 'Call' }).click();
  await expect(page.getByRole('status').filter({ hasText: /Returned|Failed/ })).toBeVisible();
});

test('a call with arguments that are not JSON is refused before it is sent', async ({ page }) => {
  await signIn(page);
  await page.goto('/tools/read_file');
  await expect(page.getByRole('heading', { name: 'read_file', level: 2 })).toBeVisible();
  await page.getByRole('textbox', { name: 'Arguments as JSON' }).fill('{not json');
  await page.getByRole('button', { name: 'Call' }).click();
  await expect(page.getByText(/Not JSON/)).toBeVisible();
});

test('plugins list with their tools, and reload', async ({ page }) => {
  await signIn(page);
  await page.goto('/tools/plugins');
  const plugins = page.getByRole('region', { name: 'Plugins' });
  await expect(plugins.getByRole('strong').filter({ hasText: /^shell$/ })).toBeVisible();
  await plugins.getByRole('button', { name: 'Reload' }).click();
  await expect(plugins.getByRole('status')).toContainText(/reloaded|Could not/);
});

test('a skill is listed and read', async ({ page }) => {
  await signIn(page);
  await page.goto('/tools/skills');
  const skills = page.getByRole('navigation', { name: 'Skills' });
  const first = skills.getByRole('link').first();
  await expect(first).toBeVisible();
  await first.click();
  await expect(page).toHaveURL(/\/tools\/skills\/[^/]+$/);
  const skill = page.getByRole('region', { name: 'Skill' });
  await expect(skill.getByRole('heading', { level: 2 })).toBeVisible();
  await expect(skill.getByText(/shipped with Nine|from the skills directory|written by Nine/)).toBeVisible();
});

test('the inbox shows what waits, the notifications and the capabilities in force', async ({ page }) => {
  await signIn(page);
  await page.goto('/inbox');
  for (const name of ['Capability requests', 'Questions', 'Notifications', 'Capabilities in force']) {
    await expect(page.getByRole('heading', { name, level: 2 })).toBeVisible();
  }
  await expect(page.getByText('No request is waiting.')).toBeVisible();
});
