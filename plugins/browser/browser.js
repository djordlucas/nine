import { chromium } from 'playwright';
import { config } from './config.js';
import { isBlockedUrl } from './security.js';

let browser = null;
let page = null;


// Every navigation is checked, not just the one the agent asked for.
// checkUrl guards the URL passed to browser_navigate, but a public page that
// 302s to 169.254.169.254 — or whose hostname simply resolves there — walks
// straight past it. Redirects are where an SSRF guard on the initial URL alone
// stops being a guard.
//
// Only navigation requests are intercepted: subresources (images, scripts) are
// left alone, so this does not turn every page load into a routing decision.
// That means a page embedding <img src="http://169.254.169.254/..."> is still
// fetched by the browser; the agent cannot read the response, but this is a
// known limit rather than a closed door.
async function guardNavigations(target) {
  await target.route('**/*', async (route, request) => {
    if (!request.isNavigationRequest()) return route.continue();
    if (isBlockedUrl(request.url())) {
      process.stderr.write(`[browser] blocked navigation to ${request.url()}\n`);
      return route.abort('blockedbyclient');
    }
    return route.continue();
  });
}

export async function ensureBrowser() {
  if (browser && !browser.isConnected()) {
    browser = null;
    page = null;
  }
  if (!browser) {
    browser = await chromium.launch({
      headless: config.headless,
      executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
      args: ['--no-sandbox', '--disable-setuid-sandbox',
             '--disable-dev-shm-usage', '--disable-gpu'],
    });
  }
  if (!page) {
    const ctx = await browser.newContext({
      viewport: { width: config.viewport.width, height: config.viewport.height },
      userAgent: 'Mozilla/5.0 (compatible; nine-agent/1.0)',
    });
    page = await ctx.newPage();
    await guardNavigations(page);
    page.on('pageerror', (err) =>
      process.stderr.write(`[browser] page error: ${err.message}\n`));
  }
  return page;
}

export async function resetBrowser() {
  if (page) {
    try { await page.context().close(); } catch {}
    page = null;
  }
  if (browser) {
    const ctx = await browser.newContext({
      viewport: { width: config.viewport.width, height: config.viewport.height },
      userAgent: 'Mozilla/5.0 (compatible; nine-agent/1.0)',
    });
    page = await ctx.newPage();
    await guardNavigations(page);
    page.on('pageerror', (err) =>
      process.stderr.write(`[browser] page error: ${err.message}\n`));
  }
  return page;
}

export function getBrowser() { return browser; }
export function getPage() { return page; }

export async function closeBrowser() {
  if (browser) {
    await browser.close().catch(() => {});
    browser = null;
    page = null;
  }
}
