import { ensureBrowser } from '../browser.js';

export async function extract(args) {
  const { selector, attribute, as_json = false, max_chars = 50000 } = args;

  const page = await ensureBrowser();

  let result;
  if (!selector) {
    result = await page.evaluate(() => document.body?.innerText ?? '');
  } else if (attribute) {
    const values = await page.$$eval(
      selector,
      (els, attr) => els.map(el => el.getAttribute(attr) ?? ''),
      attribute
    );
    result = as_json ? JSON.stringify(values) : values.join('\n');
  } else {
    const texts = await page.$$eval(selector, els => els.map(el => el.innerText ?? ''));
    result = as_json ? JSON.stringify(texts) : texts.join('\n');
  }

  if (typeof result === 'string' && result.length > max_chars) {
    result = result.slice(0, max_chars) + `\n[truncated at ${max_chars} chars]`;
  }

  return result;
}
