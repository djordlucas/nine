import { ensureBrowser } from '../browser.js';
import { checkUrl } from '../security.js';
import { config } from '../config.js';

export async function navigate(args) {
  const { url, wait_until = 'load', timeout = config.timeout } = args;
  if (!url) throw new Error('url is required');
  checkUrl(url);

  const page = await ensureBrowser();
  let resp;
  try {
    resp = await page.goto(url, { waitUntil: wait_until, timeout });
  } catch (err) {
    if (err.name === 'TimeoutError')
      throw new Error(`navigation timed out after ${timeout}ms: ${url}`);
    throw new Error(`navigation failed: ${err.message}`);
  }

  return JSON.stringify({
    url: page.url(),
    title: await page.title(),
    status: resp?.status() ?? null,
  });
}
