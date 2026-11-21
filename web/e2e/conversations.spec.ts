import type { Page } from '@playwright/test';
import { expect, signIn, test } from './fixtures';

// Phase 3 (adr/web-ui.md §9): conversations against the stub model
// (stub-model.mjs), which answers from what it is sent.

async function newConversation(page: Page) {
  await signIn(page);
  await page.goto('/conversations');
  await page.getByRole('button', { name: 'Run agent' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Start' }).click();
  await expect(page).toHaveURL(/\/conversations\/[^/]+$/);
  await expect(page.getByText('No messages yet. Say something.')).toBeVisible();
}

async function send(page: Page, text: string) {
  const box = page.getByRole('textbox', { name: 'Message' });
  await box.fill(text);
  // Enter sends only with a fine pointer; on a touch screen it is a newline
  // and the Send button submits instead.
  if (await page.evaluate(() => matchMedia('(pointer: coarse)').matches)) await page.getByRole('button', { name: 'Send' }).click();
  else await box.press('Enter');
}

const messages = (page: Page) => page.getByRole('list', { name: 'Messages' });

// On a phone the header's actions live behind the "More actions" menu.
async function openHeaderActions(page: Page, isMobile: boolean) {
  if (isMobile) await page.getByRole('button', { name: 'More actions' }).click();
}

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
  await page.getByRole('button', { name: 'Stop', exact: true }).click();
  await expect(messages(page).getByText('Cancelled.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Stop', exact: true })).toHaveCount(0);
  await send(page, 'still there?');
  await expect(messages(page).locator('strong', { hasText: 'still there?' })).toBeVisible();
});

test('the context panel shows what the next call would see', async ({ page, isMobile }) => {
  await newConversation(page);
  await openHeaderActions(page, isMobile);
  await page.getByRole('button', { name: 'Context' }).click();
  await expect(page.getByRole('region', { name: 'Context' })).toContainText('tokens');
});

test('a conversation can be deleted', async ({ page, isMobile }) => {
  await newConversation(page);
  await send(page, 'delete me soon');
  await expect(page.getByRole('button', { name: 'Inspect turn 1' })).toBeVisible();
  const url = page.url();
  await openHeaderActions(page, isMobile);
  await page.getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog', { name: 'Delete this conversation?' }).getByRole('button', { name: 'Delete' }).click();
  await expect(page).toHaveURL(/\/conversations$/);
  await page.goto(url);
  await expect(page.getByText('This conversation does not exist.')).toBeVisible();
});

test('a phone shows the list or the chat, not both', async ({ page, isMobile }) => {
  await newConversation(page);
  const list = page.getByRole('navigation', { name: 'Agents' });
  await expect(list).toBeVisible({ visible: !isMobile });
  if (isMobile) {
    await page.getByRole('link', { name: 'All agents' }).click();
    await expect(list).toBeVisible();
  }
});

test('the plan button steps through the modes for the session, and the context use always shows', async ({ page }) => {
  await newConversation(page);
  // Before any turn, the bar already shows the context the next call would take.
  await expect(page.getByText(/^ctx \d/)).toBeVisible();
  const plan = page.getByRole('button', { name: /^Plan: / });
  await expect(plan).toBeVisible();
  for (let i = 0; i < 3 && (await plan.getAttribute('aria-label')) !== 'Plan: always'; i++) {
    const before = await plan.getAttribute('aria-label');
    await plan.click();
    await expect(plan).not.toHaveAttribute('aria-label', before ?? '');
  }
  await expect(plan).toHaveAttribute('aria-label', 'Plan: always');
  await page.reload();
  await expect(page.getByRole('button', { name: 'Plan: always' })).toBeVisible();
  await send(page, 'hello there');
  await expect(page.getByRole('button', { name: 'Inspect turn 1' })).toBeVisible();
  await expect(page.getByText(/^ctx \d/)).toBeVisible();
});

test('on a phone the chat sits above the details toggle; on a desktop the panel shows beside it', async ({ page, isMobile }) => {
  await newConversation(page);
  const composer = page.getByRole('textbox', { name: 'Message' });
  const toggle = page.getByRole('button', { name: 'Details', exact: true });
  const side = page.getByRole('complementary', { name: 'Agent details' });
  if (isMobile) {
    await expect(toggle).toBeVisible();
    await expect(side).toBeHidden();
    expect((await composer.boundingBox())!.y).toBeLessThan((await toggle.boundingBox())!.y);
    await toggle.click();
    await expect(side).toBeVisible();
  } else {
    await expect(toggle).toBeHidden();
    await expect(side).toBeVisible();
  }
});

test('the header shows one set of actions per layout: an icon row on a desktop, a menu on a phone', async ({ page, isMobile }) => {
  await newConversation(page);
  const more = page.getByRole('button', { name: 'More actions' });
  if (isMobile) {
    await expect(more).toBeVisible();
    // Until the menu opens, the actions are not in the tree.
    await expect(page.getByRole('button', { name: 'Context' })).toHaveCount(0);
    await more.click();
    await expect(page.getByRole('button', { name: 'Context' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'End session' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Delete', exact: true })).toBeVisible();
  } else {
    await expect(more).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Context' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'End session' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Delete', exact: true })).toBeVisible();
  }
});

test('scrolling up reveals a button that jumps back to the latest message', async ({ page, isMobile }) => {
  await newConversation(page);
  for (let i = 1; i <= 12; i++) {
    await send(page, `fill the page ${i}`);
    await expect(messages(page).locator('strong', { hasText: `fill the page ${i}` })).toBeVisible();
  }
  // The last turn is over: nothing streams in to pull the view back down.
  await expect(page.getByRole('button', { name: 'Stop', exact: true })).toHaveCount(0);
  const jump = page.getByRole('button', { name: 'Scroll to the latest message' });
  await expect(jump).toHaveCount(0);
  // Scroll the topmost container that actually scrolls (the message list on a
  // desktop, some outer pane on a phone) back to the top.
  await messages(page).evaluate((el) => {
    let p: HTMLElement | null = el as HTMLElement;
    while (p && p.scrollHeight <= p.clientHeight + 1) p = p.parentElement;
    if (p) p.scrollTop = 0;
    window.scrollTo(0, 0);
  });
  await expect(jump).toBeVisible();
  await jump.click();
  await expect(jump).toHaveCount(0);
});

test('the details panel shows context use and links out to the journal and the role', async ({ page, isMobile }) => {
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
  await newConversation(page);
  await send(page, 'hello there');
  await expect(page.getByRole('button', { name: 'Inspect turn 1' })).toBeVisible();
  const side = page.getByRole('complementary', { name: 'Agent details' });
  if (isMobile) await page.getByRole('button', { name: 'Details', exact: true }).click();
  await expect(side.getByText(/^context \d+%$/)).toBeVisible();
  const id = page.url().split('/').pop()!;
  const journal = side.getByRole('link', { name: 'View in Journal' });
  await expect(journal).toHaveAttribute('href', `/journal?agent=${id}`);
  await side.getByRole('button', { name: 'Copy id' }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as { __copied: string }).__copied)).toBe(id);
});
