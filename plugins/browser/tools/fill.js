import { ensureBrowser } from '../browser.js';
import { config } from '../config.js';

export async function fill(args) {
  const { selector, value, clear_first = true, timeout = config.timeout } = args;
  if (!selector) throw new Error('selector is required');
  if (value == null) throw new Error('value is required');

  const page = await ensureBrowser();

  try {
    if (clear_first) {
      await page.fill(selector, value, { timeout });
    } else {
      await page.type(selector, value, { timeout });
    }
  } catch (err) {
    if (err.name === 'TimeoutError')
      throw new Error(`element not found within ${timeout}ms: ${selector}`);
    throw new Error(`fill failed: ${err.message}`);
  }

  return 'ok';
}
