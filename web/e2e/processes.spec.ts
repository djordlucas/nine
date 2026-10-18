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
const state = (page: Page) => page.getByRole('region', { name: 'Process' }).locator('header');

test('a process is stopped and started from its page', async ({ page, request }) => {
  const id = await createGoal(request, `e2e process ${Date.now()}`);
  try {
    await signIn(page);
    await page.goto('/processes');
    await expect(page.getByText(/of \d+ running/)).toBeVisible();
    await page.getByRole('navigation', { name: 'Processes' }).getByRole('link', { name: new RegExp(`goal:${id}`) }).click();
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
    await page.goto('/goals');
    await page.getByRole('navigation', { name: 'Goals' }).getByRole('link', { name: description }).click();
    await expect(page.getByRole('heading', { name: description, level: 2 })).toBeVisible();
    const procs = page.getByRole('region', { name: 'Goal' }).getByRole('list').first();
    await expect(procs.getByRole('link', { name: `goal:${id}` })).toBeVisible();

    await page.getByRole('button', { name: 'Pause' }).click();
    await expect(page.getByText('Now paused.')).toBeVisible();
    await expect(procs).toContainText('stopped');

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

test('a goal and a sub-goal are created and deleted from the page', async ({ page }) => {
  await signIn(page);
  await page.goto('/goals');
  const description = `e2e created ${Date.now()}`;
  await page.getByRole('textbox', { name: 'New goal' }).fill(description);
  await page.getByRole('button', { name: 'Create' }).click();
  await expect(page.getByRole('heading', { name: description, level: 2 })).toBeVisible();

  const step = `a first step ${Date.now()}`;
  await page.getByRole('textbox', { name: 'New sub-goal' }).fill(step);
  await page.getByRole('button', { name: 'Add' }).click();
  // The goal's own list, which a phone shows in place of the goal list.
  await expect(page.getByRole('region', { name: 'Sub-goals' }).getByRole('link', { name: step })).toBeVisible();

  await page.getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click();
  await expect(page).toHaveURL(/\/goals$/);
  const goals = page.getByRole('navigation', { name: 'Goals' });
  await expect(goals.getByRole('link', { name: description })).toHaveCount(0);
  await expect(goals.getByRole('link', { name: step })).toHaveCount(0);
});

test('the goals page lists workflows, or says there are none', async ({ page }) => {
  await signIn(page);
  await page.goto('/goals');
  const workflows = page.getByRole('navigation', { name: 'Workflows' });
  await expect(workflows.getByRole('heading', { name: 'Workflows' })).toBeVisible();
  await expect(workflows.getByText(/No workflows|steps/).first()).toBeVisible();
});
