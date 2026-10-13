import { defineConfig, devices } from '@playwright/test';

// Runs inside e2e/compose.yaml against the runtime image (adr/web-ui.md §8):
// one phone and one desktop viewport.
export default defineConfig({
  testDir: 'e2e',
  globalSetup: './e2e/wait-for-nine.ts',
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: [['list']],
  use: {
    baseURL: process.env.NINE_E2E_URL ?? 'http://localhost:8080',
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'phone', use: { ...devices['Pixel 7'], viewport: { width: 390, height: 844 } } },
    { name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } },
  ],
});
