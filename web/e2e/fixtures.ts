// Shared by every spec: a clean-console check and signing in.
import { expect, test as base, type Page } from '@playwright/test';

export const token = process.env.NINE_E2E_TOKEN ?? 'e2e-token';

// Every page must load without a CSP violation or an uncaught error. A failed
// request is logged by the browser too; those are what some tests provoke.
export const test = base.extend<{ clean: void }>({
  clean: [
    async ({ page }, use) => {
      const problems: string[] = [];
      page.on('console', (m) => {
        if (m.type() === 'error' && !m.text().startsWith('Failed to load resource')) problems.push(m.text());
      });
      page.on('pageerror', (e) => problems.push(e.message));
      await use();
      expect(problems).toEqual([]);
    },
    { auto: true },
  ],
});

export async function signIn(page: Page) {
  await page.goto('/');
  await page.getByLabel('API token').fill(token);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Dashboard', level: 1 })).toBeVisible();
}

export { expect };
