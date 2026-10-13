import { defineConfig } from 'vitest/config';
import preact from '@preact/preset-vite';

// The build lands in internal/webui/dist, which the Go binary embeds and the
// repository commits (adr/web-ui.md §2). `npm run dev` proxies the API to a
// daemon on :8080.
export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    // No inline script or data: modules: the served CSP allows 'self' only.
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    sourcemap: false,
  },
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
  },
});
