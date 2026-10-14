import { expect, signIn, test } from './fixtures';

test('a wrong token is refused', async ({ page }) => {
  await page.goto('/');
  await page.getByLabel('API token').fill('not-the-token');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('alert')).toContainText('not the one');
});

test('signing in shows the dashboard with the daemon status', async ({ page }) => {
  await signIn(page);
  const daemon = page.getByRole('region', { name: 'Daemon' });
  await expect(daemon.getByText('Version')).toBeVisible();
  await expect(daemon.getByText('Uptime')).toBeVisible();
});

test('the session survives a reload and a deep link', async ({ page }) => {
  await signIn(page);
  await page.goto('/processes');
  await expect(page.getByRole('heading', { name: 'Processes', level: 1 })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Processes', level: 1 })).toBeVisible();
});

test('the layout fits the viewport', async ({ page }) => {
  await signIn(page);
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});

test('every section is reachable', async ({ page, isMobile }) => {
  await signIn(page);
  const sections = ['Conversations', 'Processes', 'Goals & workflows', 'Tools', 'Inbox', 'Journal', 'System'];
  for (const name of sections) {
    if (isMobile) {
      const tab = page.getByRole('navigation', { name: 'Sections' }).getByRole('link', { name, exact: true });
      if (await tab.isVisible()) {
        await tab.click();
      } else {
        await page.getByRole('button', { name: 'More' }).click();
        await page.getByRole('dialog', { name: 'More sections' }).getByRole('link', { name }).click();
      }
    } else {
      await page.getByRole('navigation', { name: 'Sections' }).getByRole('link', { name, exact: true }).click();
    }
    await expect(page.getByRole('heading', { name, level: 1 })).toBeVisible();
  }
});

test('the rail shows on desktop and the tab bar on a phone', async ({ page, isMobile }) => {
  await signIn(page);
  const navs = page.getByRole('navigation', { name: 'Sections' });
  await expect(navs.filter({ has: page.getByRole('button', { name: 'More' }) })).toBeVisible({ visible: isMobile });
  await expect(navs.filter({ has: page.getByRole('link', { name: 'Nine' }) })).toBeVisible({ visible: !isMobile });
});

test('signing out returns to the sign-in page and ends the session', async ({ page, isMobile }) => {
  await signIn(page);
  if (isMobile) await page.getByRole('button', { name: 'More' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page.getByLabel('API token')).toBeVisible();
  await page.goto('/');
  await expect(page.getByLabel('API token')).toBeVisible();
});

test('the API refuses a cookie write without the CSRF header', async ({ page }) => {
  await signIn(page);
  const status = await page.evaluate(async () => (await fetch('/api/v1/plugins/reload', { method: 'POST' })).status);
  expect(status).toBe(403);
});
