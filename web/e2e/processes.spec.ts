import type { APIRequestContext, Page } from '@playwright/test';
import { expect, signIn, test, token } from './fixtures';

// Phase 4 (adr/web-ui.md §9): processes, goals and workflows. A top-level goal
// created through the API gets a pursue process, goal:<id>, which the tests
// stop, start and pause through its goal.

const bearer = { Authorization: `Bearer ${token}` };

async function createGoal(request: APIRequestContext, description: string): Promise<string> {
  const res = await request.post('/api/v1/goals', { headers: bearer, data: { description } });
  expect(res.status()).toBe(201);
  return (await res.json()).goal.id as string;
}

async function deleteGoal(request: APIRequestContext, id: string) {
  await request.delete(`/api/v1/goals/${id}`, { headers: bearer });
}

const processPage = (id: string) => `/processes/${encodeURIComponent(`goal:${id}`)}`;
const state = (page: Page) => page.getByRole('complementary', { name: 'Agent details' }).locator('header');

test('a process is stopped and started from its page', async ({ page, request }) => {
  const description = `e2e process ${Date.now()}`;
  const id = await createGoal(request, description);
  try {
    await signIn(page);
    await page.goto('/processes');
    await expect(page.getByText(/of \d+ running/)).toBeVisible();
    await page.getByRole('navigation', { name: 'Agents' }).getByRole('link', { name: new RegExp(description) }).click();
    await expect(page.getByRole('heading', { name: `goal:${id}`, level: 2 })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Budget today' })).toBeVisible();

    await page.getByRole('button', { name: 'Stop' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Stop' }).click();
    await expect(state(page)).toContainText('stopped');
    await expect(page.getByText('Stopped by the operator.')).toBeVisible();

    await page.getByRole('button', { name: 'Start' }).click();
    await expect(state(page)).toContainText('running');
  } finally {
    await deleteGoal(request, id);
  }
});

test('pausing a goal stops its process, and resuming it starts it again', async ({ page, request }) => {
  const description = `e2e goal pause ${Date.now()}`;
  const id = await createGoal(request, description);
  try {
    await signIn(page);
    // A goal shows on the Agents page attached to its pursue process.
    await page.goto('/conversations');
    const agents = page.getByRole('navigation', { name: 'Agents' });
    await expect(agents.getByRole('link', { name: new RegExp(description) })).toBeVisible();

    // Pause the goal through the API; its process stops.
    const pause = await request.post(`/api/v1/goals/${id}/status`, { headers: bearer, data: { status: 'paused' } });
    expect(pause.status()).toBe(200);

    // The process page sends a goal-stopped process back to its goal.
    await page.goto(processPage(id));
    await expect(page.getByText(/Stopped by its goal/)).toBeVisible();
    await expect(page.getByRole('button', { name: 'Start' })).toHaveCount(0);
    await page.getByRole('button', { name: 'Resume goal' }).click();
    await expect(state(page)).toContainText('running');
  } finally {
    await deleteGoal(request, id);
  }
});

test('a goal created through the API appears on the Agents page, attached to its process', async ({ page, request }) => {
  const description = `e2e agent goal ${Date.now()}`;
  const id = await createGoal(request, description);
  try {
    await signIn(page);
    await page.goto('/conversations');
    const row = page.getByRole('navigation', { name: 'Agents' }).getByRole('link', { name: new RegExp(description) });
    await expect(row).toBeVisible();
    await expect(row).toContainText('goal');
    // The old /goals/:id link lands on the goal's process.
    await page.goto(`/goals/${id}`);
    await expect(page).toHaveURL(new RegExp(`/processes/goal%3A${id}$`));
    await expect(page.getByRole('heading', { name: `goal:${id}`, level: 2 })).toBeVisible();
  } finally {
    await deleteGoal(request, id);
  }
});

test('a process agent’s activity is its session, read-only', async ({ page, request }) => {
  const id = await createGoal(request, `e2e watch ${Date.now()}`);
  try {
    await signIn(page);
    await page.goto(processPage(id));
    const pane = page.getByRole('region', { name: 'Activity' });
    // Its session has an activity once the process has run a turn; until then the pane says so.
    await expect(pane.getByRole('list', { name: 'Messages' }).or(pane.getByText('has not run a turn yet'))).toBeVisible();
    // Read-only: no composer, no delete. (exact: the panel's "Message to the
    // process" input must not match.)
    await expect(page.getByRole('textbox', { name: 'Message', exact: true })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Delete', exact: true })).toHaveCount(0);
    // The process’s specifics sit in the panel on the right.
    const side = page.getByRole('complementary', { name: 'Agent details' });
    await expect(side.getByRole('heading', { name: `goal:${id}`, level: 2 })).toBeVisible();
    await expect(side.getByText('Budget today')).toBeVisible();
  } finally {
    await deleteGoal(request, id);
  }
});
