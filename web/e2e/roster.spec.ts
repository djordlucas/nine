import type { Page } from '@playwright/test';
import { expect, signIn, test } from './fixtures';

// The roster on the Agents page: text filter, type chips, attention badges,
// and row-level rename/delete.

async function send(page: Page, text: string) {
  const box = page.getByRole('textbox', { name: 'Message' });
  await box.fill(text);
  // Enter sends only with a fine pointer; on a touch screen it is a newline
  // and the Send button submits instead.
  if (await page.evaluate(() => matchMedia('(pointer: coarse)').matches)) await page.getByRole('button', { name: 'Send' }).click();
  else await box.press('Enter');
}

/** Starts a conversation whose first turn names it after the message. */
async function newNamed(page: Page, text: string) {
  await page.getByRole('button', { name: 'New agent' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Start' }).click();
  await expect(page).toHaveURL(/\/conversations\/[^/]+$/);
  const id = page.url().split('/conversations/')[1];
  await send(page, text);
  // The daemon slugs the first message: non-alphanumerics are dropped, spaces become dashes.
  const name = text.toLowerCase().replace(/[^a-z0-9 ]/g, '').trim().replace(/\s+/g, '-');
  await expect(page.getByRole('heading', { level: 2, name })).toBeVisible();
  await page.goto('/conversations');
  return { id, name };
}

const roster = (page: Page) => page.getByRole('navigation', { name: 'Agents' });

test('the roster filters by text and by type chip', async ({ page }) => {
  await signIn(page);
  await page.goto('/conversations');
  const stamp = Date.now();
  const alpha = await newNamed(page, `hello alpha ${stamp}`);
  const beta = await newNamed(page, `hello beta ${stamp}`);
  const list = roster(page);
  const alphaRow = list.getByRole('link', { name: new RegExp(alpha.name) });
  const betaRow = list.getByRole('link', { name: new RegExp(beta.name) });
  await expect(alphaRow).toBeVisible();
  await expect(betaRow).toBeVisible();

  // Text filter narrows to the match.
  await page.getByRole('searchbox', { name: 'Filter agents' }).fill('alpha');
  await expect(alphaRow).toBeVisible();
  await expect(betaRow).toHaveCount(0);
  await page.getByRole('searchbox', { name: 'Filter agents' }).fill('');

  // The type chips swap conversations for processes and back.
  await page.getByRole('button', { name: 'Processes', exact: true }).click();
  await expect(alphaRow).toHaveCount(0);
  await expect(list.getByRole('link', { name: /self-reflection/ })).toBeVisible();
  await page.getByRole('button', { name: 'Conversations', exact: true }).click();
  await expect(alphaRow).toBeVisible();
  await expect(list.getByRole('link', { name: /self-reflection/ })).toHaveCount(0);
  await page.getByRole('button', { name: 'All', exact: true }).click();
  await expect(alphaRow).toBeVisible();
  await expect(list.getByRole('link', { name: /self-reflection/ })).toBeVisible();
});

test('a row renames and deletes from the roster', async ({ page }) => {
  await signIn(page);
  await page.goto('/conversations');
  const stamp = Date.now();
  const { name } = await newNamed(page, `hello rename me ${stamp}`);
  const list = roster(page);
  const row = list.getByRole('link', { name: new RegExp(name) });
  await expect(row).toBeVisible();
  const renamed = `renamed ${stamp}`;

  // Rename from the row's action, through the dialog.
  await page.getByRole('button', { name: `Rename ${name}` }).click();
  await page.getByRole('dialog').getByLabel('Name').fill(renamed);
  await page.getByRole('dialog').getByRole('button', { name: 'Save' }).click();
  await expect(list.getByRole('link', { name: new RegExp(renamed) })).toBeVisible();
  await expect(row).toHaveCount(0);

  // Delete from the row's action, through the confirm dialog.
  await page.getByRole('button', { name: `Delete ${renamed}` }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
  await expect(list.getByRole('link', { name: new RegExp(renamed) })).toHaveCount(0);
});

test('a pending question badges the conversation row', async ({ page }) => {
  await signIn(page);
  await page.goto('/conversations');
  const stamp = Date.now();
  const { id, name } = await newNamed(page, `hello question ${stamp}`);
  // The stub model answers "ask me ..." with an ask_human question.
  await roster(page).getByRole('link', { name: new RegExp(name) }).click();
  await send(page, 'ask me again please');
  await expect(page.getByRole('group', { name: 'Question from the session' })).toBeVisible({ timeout: 10000 });
  await page.goto('/conversations');
  // The row badges the pending question. (The turn is not over, so the
  // conversation may still carry its pre-rename name — find it by href.)
  const row = roster(page).locator(`a[href="/conversations/${id}"]`);
  await expect(row).toBeVisible({ timeout: 10000 });
  await expect(row).toContainText('?', { timeout: 10000 });
});
