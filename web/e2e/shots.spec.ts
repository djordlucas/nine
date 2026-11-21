import { mkdirSync } from 'node:fs';
import { expect, signIn, test } from './fixtures';

// The visual record: every section in both themes on both layouts, saved under
// e2e/shots/ and committed, so a style change shows up in the PR diff and a
// regression is visible at a glance. Not a pixel comparison — CI just reruns
// it; the PNGs are for review.

const pages = ['/conversations', '/builder', '/inbox', '/data/files', '/journal', '/system'];

const slug = (path: string) => path.replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'root';

test('visual record of every section, both themes', async ({ page }) => {
  mkdirSync('e2e/shots', { recursive: true });
  await signIn(page);
  // A conversation with a turn, so the chat page has content to show.
  await page.goto('/conversations');
  await page.getByRole('button', { name: 'New agent' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Start' }).click();
  await expect(page).toHaveURL(/\/conversations\/[^/]+$/);
  const box = page.getByRole('textbox', { name: 'Message' });
  await box.fill('hello there');
  if (await page.evaluate(() => matchMedia('(pointer: coarse)').matches)) await page.getByRole('button', { name: 'Send' }).click();
  else await box.press('Enter');
  await expect(page.getByRole('button', { name: 'Inspect turn 1' })).toBeVisible();
  const chatPath = page.url().replace(/^.*(?=\/conversations)/, '');

  for (const theme of ['light', 'dark']) {
    for (const path of [...pages, chatPath]) {
      await page.goto(path);
      // The theme signal owns data-theme; set it after each navigation.
      await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
      await page.waitForTimeout(350);
      await page.screenshot({ path: `e2e/shots/${test.info().project.name}-${theme}-${slug(path)}.png` });
    }
  }
});
