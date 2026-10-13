import { signal, effect } from '@preact/signals';

/** `system` follows prefers-color-scheme; the others override it. */
export type Theme = 'system' | 'light' | 'dark';

const KEY = 'nine.theme';

function stored(): Theme {
  try {
    const v = localStorage.getItem(KEY);
    if (v === 'light' || v === 'dark') return v;
  } catch {
    // Storage can be unavailable (private mode, blocked site data).
  }
  return 'system';
}

export const theme = signal<Theme>(stored());

effect(() => {
  const t = theme.value;
  if (t === 'system') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme', t);
  try {
    if (t === 'system') localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, t);
  } catch {
    // A theme that does not persist still applies to this page.
  }
});

export function cycleTheme() {
  theme.value = theme.value === 'system' ? 'light' : theme.value === 'light' ? 'dark' : 'system';
}
