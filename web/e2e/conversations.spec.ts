import type { Page } from '@playwright/test';
import { expect, signIn, test } from './fixtures';

// Phase 3 (adr/web-ui.md §9): conversations against the stub model
// (stub-model.mjs), which answers from what it is sent.

async function newConversation(page: Page) {
  await signIn(page);
  await page.goto('/conversations');
  await page.getByRole('button', { name: 'New conversation' }).click();
  await expect(page).toHaveURL(/\/conversations\/[^/]+$/);
  await expect(page.getByText('No messages yet. Say something.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Send' })).toBeDisabled();
}

async function send(page: Page, text: string) {
  await page.getByRole('textbox', { name: 'Message' }).fill(text);
  await page.getByRole('button', { name: 'Send' }).click();
}

const messages = (page: Page) => page.getByRole('list', { name: 'Messages' });

test('a conversation streams its reply and renders Markdown', async ({ page }) => {
  await newConversation(page);
  await send(page, 'hello there');
  const list = messages(page);
  await expect(list.getByText('hello there', { exact: true }).first()).toBeVisible();
  await expect(list.locator('strong', { hasText: 'hello there' })).toBeVisible();
  await expect(list.getByRole('listitem').filter({ hasText: /^one$/ })).toBeVisible();
  await expect(list.locator('code', { hasText: 'code' })).toBeVisible();
  // Once the turn is over the transcript numbers it.
  await expect(page.getByRole('button', { name: 'Inspect turn 1' })).toBeVisible();
  // The first turn names the conversation, and the title shows the name.
  await expect(page.getByRole('heading', { level: 2, name: 'hello-there' })).toBeVisible();
});

test('a tool call shows in the chat and in the turn inspector', async ({ page }) => {
  await newConversation(page);
  await send(page, 'please use a tool');
  await expect(messages(page).locator('summary', { hasText: 'skill_list' })).toBeVisible();
  await expect(messages(page).getByText('answered.')).toBeVisible();

  // The inspector opens the turn's record in place, and closes it again.
  const inspect = page.getByRole('button', { name: 'Inspect turn 1' });
  await inspect.click();
  await expect(inspect).toHaveAttribute('aria-expanded', 'true');
  const turn = page.getByRole('region', { name: 'Turn 1' });
  await expect(turn.getByRole('region', { name: 'Input' })).toContainText('please use a tool');
  await expect(turn.getByRole('region', { name: 'Tool calls' })).toContainText('skill_list');
  await expect(turn.getByRole('region', { name: 'Model calls' })).toContainText('Call 2');
  await expect(turn.getByRole('region', { name: 'Result' })).toContainText('answered');
  await turn.getByText(/^Raw events/).click();
  await expect(turn.getByText('turn_start').first()).toBeVisible();
  await inspect.click();
  await expect(turn).toHaveCount(0);
  await expect(page).toHaveURL(/\/conversations\/[^/]+$/);
});

test('the session’s question is answered in the chat', async ({ page }) => {
  await newConversation(page);
  await send(page, 'ask me something');
  const q = page.getByRole('group', { name: 'Question from the session' });
  await expect(q).toContainText('Which colour?');
  await q.getByRole('button', { name: 'Blue' }).click();
  await expect(q).toContainText('Answered: Blue');
  await expect(messages(page).locator('strong', { hasText: 'Blue' })).toBeVisible();
});

test('a running turn can be stopped, and the session carries on', async ({ page }) => {
  await newConversation(page);
  await send(page, 'go slow');
  await expect(messages(page).getByText(/tick 2/)).toBeVisible();
  await page.getByRole('button', { name: 'Stop' }).click();
  await expect(messages(page).getByText('Cancelled.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Stop' })).toHaveCount(0);
  await send(page, 'still there?');
  await expect(messages(page).locator('strong', { hasText: 'still there?' })).toBeVisible();
});

test('the context panel shows what the next call would see', async ({ page }) => {
  await newConversation(page);
  await page.getByRole('button', { name: 'Context' }).click();
  await expect(page.getByRole('region', { name: 'Context' })).toContainText('tokens');
});

test('a conversation can be deleted', async ({ page }) => {
  await newConversation(page);
  await send(page, 'delete me soon');
  await expect(page.getByRole('button', { name: 'Inspect turn 1' })).toBeVisible();
  const url = page.url();
  await page.getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog', { name: 'Delete this conversation?' }).getByRole('button', { name: 'Delete' }).click();
  await expect(page).toHaveURL(/\/conversations$/);
  await page.goto(url);
  await expect(page.getByText('This conversation does not exist.')).toBeVisible();
});

test('a phone shows the list or the chat, not both', async ({ page, isMobile }) => {
  await newConversation(page);
  const list = page.getByRole('navigation', { name: 'Conversations' });
  await expect(list).toBeVisible({ visible: !isMobile });
  if (isMobile) {
    await page.getByRole('link', { name: 'All conversations' }).click();
    await expect(list).toBeVisible();
  }
});
