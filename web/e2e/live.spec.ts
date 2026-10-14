import type { APIRequestContext } from '@playwright/test';
import { expect, signIn, test, token } from './fixtures';

// Phase 2 (adr/web-ui.md §9): the dashboard and the Journal stay current from
// the event stream. A change made elsewhere, through the API with the bearer
// token, must reach an open page without a reload.

const bearer = { Authorization: `Bearer ${token}` };

async function createGoal(request: APIRequestContext, description: string): Promise<string> {
  const res = await request.post('/api/v1/goals', { headers: bearer, data: { description } });
  expect(res.status()).toBe(201);
  const body = await res.json();
  return body.goal.id as string;
}

async function deleteGoal(request: APIRequestContext, id: string) {
  await request.delete(`/api/v1/goals/${id}`, { headers: bearer });
}

test('the dashboard shows its tiles and goes live', async ({ page }) => {
  await signIn(page);
  for (const name of ['Needs you', 'Processes', 'Activity', 'Recent conversations', 'Goals & workflows', 'Notifications', 'Daemon']) {
    await expect(page.getByRole('region', { name })).toBeVisible();
  }
  await expect(page.getByRole('status').filter({ hasText: 'Live' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Needs you' })).toContainText('Nothing is waiting for you.');
});

test('a goal created elsewhere reaches the dashboard and the journal live', async ({ page, request }) => {
  await signIn(page);
  await expect(page.getByRole('status').filter({ hasText: 'Live' })).toBeVisible();

  const description = `e2e goal ${Date.now()}`;
  const id = await createGoal(request, description);
  try {
    await expect(page.getByRole('region', { name: 'Goals & workflows' })).toContainText(description);
    await expect(page.getByRole('region', { name: 'Activity' })).toContainText('goal_created');

    // In-app navigation keeps the tab's stream and what it has kept.
    await page.getByRole('link', { name: 'Open Activity' }).click();
    await expect(page.getByRole('heading', { name: 'Journal', level: 1 })).toBeVisible();
    await page.getByLabel('Show').selectOption('state');
    const row = page.getByRole('list', { name: 'Journal events' }).getByRole('button', { name: new RegExp(`goal_created.*${description}`) });
    await expect(row).toBeVisible();
    await row.click();
    await expect(page.getByText(`"description": "${description}"`)).toBeVisible();

    // A narrow filter that matches nothing says so.
    await page.getByLabel('Session or process').fill('no-such-session');
    await expect(page.getByText('No kept event matches.')).toBeVisible();
  } finally {
    await deleteGoal(request, id);
  }
  await page.getByLabel('Session or process').fill(id);
  await expect(page.getByRole('list', { name: 'Journal events' }).getByRole('button', { name: /goal_deleted/ })).toBeVisible();
});

test('the journal loads recent events on its own page', async ({ page, request }) => {
  const id = await createGoal(request, `e2e tail ${Date.now()}`);
  await deleteGoal(request, id);
  await signIn(page);
  await page.goto('/journal');
  await expect(page.getByRole('list', { name: 'Journal events' }).getByRole('button', { name: /goal_deleted/ }).first()).toBeVisible();
});

test('pausing the journal holds new events until resumed', async ({ page, request }) => {
  await signIn(page);
  await page.goto('/journal');
  await expect(page.getByRole('status').filter({ hasText: 'Live' })).toBeVisible();
  await page.getByRole('button', { name: 'Pause', exact: true }).click();
  const description = `e2e paused ${Date.now()}`;
  const id = await createGoal(request, description);
  try {
    await expect(page.getByRole('button', { name: /Resume \(\d+ new\)/ })).toBeVisible();
    const list = page.getByRole('list', { name: 'Journal events' });
    await expect(list.getByRole('button', { name: new RegExp(description) })).toHaveCount(0);
    await page.getByRole('button', { name: /^Resume/ }).click();
    await expect(list.getByRole('button', { name: new RegExp(description) })).toBeVisible();
  } finally {
    await deleteGoal(request, id);
  }
});
