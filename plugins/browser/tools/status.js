import { getBrowser, getPage } from '../browser.js';
import { config } from '../config.js';

export async function status(_args) {
  const browser = getBrowser();
  const page = getPage();

  if (!browser || !browser.isConnected() || !page) {
    return JSON.stringify({ open: false });
  }

  return JSON.stringify({
    open: true,
    url: page.url(),
    title: await page.title(),
    viewport: { width: config.viewport.width, height: config.viewport.height },
  });
}
