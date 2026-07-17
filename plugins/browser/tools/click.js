import { ensureBrowser } from '../browser.js';
import { config } from '../config.js';

export async function click(args) {
  const { selector, timeout = config.timeout, wait_for_navigation = false } = args;
  if (!selector) throw new Error('selector is required');

  const page = await ensureBrowser();

  try {
    if (wait_for_navigation) {
      await Promise.all([
        page.waitForNavigation({ timeout }),
        page.click(selector, { timeout }),
      ]);
    } else {
      await page.click(selector, { timeout });
    }
  } catch (err) {
    if (err.name === 'TimeoutError')
      throw new Error(`element not found within ${timeout}ms: ${selector}`);
    throw new Error(`click failed: ${err.message}`);
  }

  return JSON.stringify({ url: page.url(), title: await page.title() });
}
