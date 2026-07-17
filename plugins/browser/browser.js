import { chromium } from 'playwright';
import { config } from './config.js';

let browser = null;
let page = null;

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
