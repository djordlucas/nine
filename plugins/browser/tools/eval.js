import { ensureBrowser } from '../browser.js';
import { config } from '../config.js';

export async function evaluate(args) {
  const { script, timeout = config.timeout } = args;
  if (!script) throw new Error('script is required');

  const page = await ensureBrowser();

  let result;
  try {
    result = await page.evaluate(new Function(script));
  } catch (err) {
    throw new Error(`eval failed: ${err.message}`);
  }

  return JSON.stringify(result ?? null);
}
