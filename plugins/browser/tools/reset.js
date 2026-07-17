import { resetBrowser } from '../browser.js';

export async function reset(_args) {
  await resetBrowser();
  return 'ok';
}
