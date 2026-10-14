// The image's daemon and API server start in the background; wait until the
// API answers before any test runs.
export default async function waitForNine() {
  const url = `${process.env.NINE_E2E_URL ?? 'http://localhost:8080'}/api/v1/auth/session`;
  const deadline = Date.now() + 90_000;
  let last = '';
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
      last = `status ${res.status}`;
    } catch (e) {
      last = String(e);
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(`nine did not answer at ${url}: ${last}`);
}
