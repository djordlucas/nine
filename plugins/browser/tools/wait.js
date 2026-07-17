import { ensureBrowser } from '../browser.js';
import { config } from '../config.js';

export async function wait(args) {
  const { selector, text, url_pattern, timeout = config.timeout } = args;

  if (!selector && !text && !url_pattern)
    throw new Error('at least one of selector, text, or url_pattern is required');

  const page = await ensureBrowser();

  try {
    if (selector) {
      await page.waitForSelector(selector, { state: 'visible', timeout });
    } else if (text) {
      await page.waitForFunction(
        (t) => document.body?.innerText?.includes(t),
        text,
        { timeout }
      );
    } else {
      await page.waitForURL(url_pattern, { timeout });
    }
  } catch (err) {
    if (err.name === 'TimeoutError') {
      const cond = selector ? `selector "${selector}"` :
                   text     ? `text "${text}"` :
                              `URL pattern "${url_pattern}"`;
      throw new Error(`timed out after ${timeout}ms waiting for ${cond}`);
    }
    throw new Error(`wait failed: ${err.message}`);
  }

  return JSON.stringify({ url: page.url(), title: await page.title() });
}
