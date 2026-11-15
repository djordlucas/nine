import type { ComponentChildren } from 'preact';
import { signal } from '@preact/signals';

/**
 * Extra content a page puts at the right of the shell's header row (the
 * section label under the top bar). The Agents page shows its counts there;
 * null on every other page.
 */
export const headerSlot = signal<ComponentChildren>(null);
