const env = (key, def) => process.env[key] ?? def;
const envInt = (key, def) => parseInt(env(key, String(def)), 10);
const envBool = (key, def) => env(key, def ? '1' : '0') === '1';
const envList = (key) => {
  const v = process.env[key];
  return v ? v.split(',').map(s => s.trim()).filter(Boolean) : [];
};

export const config = {
  headless:          envBool('BROWSER_HEADLESS', true),
  timeout:           envInt('BROWSER_TIMEOUT', 30000),
  viewport: {
    width:           envInt('BROWSER_VIEWPORT_WIDTH', 1280),
    height:          envInt('BROWSER_VIEWPORT_HEIGHT', 800),
  },
  allowUrls:         envList('BROWSER_ALLOW_URLS'),
  blockUrls:         envList('BROWSER_BLOCK_URLS'),
  allowPrivate:      envBool('BROWSER_ALLOW_PRIVATE', false),
  maxScreenshotBytes: envInt('BROWSER_MAX_SCREENSHOT_BYTES', 3000000),
};
