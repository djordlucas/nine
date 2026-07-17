import { ensureBrowser } from '../browser.js';
import { config } from '../config.js';

export async function screenshot(args) {
  const { full_page = false, selector, quality } = args;

  const page = await ensureBrowser();
  const target = selector ? page.locator(selector).first() : page;

  let type = quality != null ? 'jpeg' : 'png';
  let opts = { type, fullPage: full_page };
  if (quality != null) opts.quality = quality;

  let buf = await target.screenshot(opts);

  if (type === 'png' && buf.length > config.maxScreenshotBytes) {
    buf = await target.screenshot({ type: 'jpeg', quality: 80, fullPage: full_page });
    type = 'jpeg';
  }

  if (buf.length > config.maxScreenshotBytes) {
    throw new Error(
      `screenshot too large (${buf.length}B). Use a selector or reduce the viewport.`
    );
  }

  const vp = page.viewportSize();
  return JSON.stringify({
    data: buf.toString('base64'),
    type,
    width: vp?.width ?? 0,
    height: vp?.height ?? 0,
  });
}
