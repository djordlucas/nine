import type { JSX } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';

/**
 * A text box as tall as its text, up to max px, past which it scrolls; the
 * chat's message box, for every other form. Nothing to drag to resize.
 */
export function Area({ max, ...props }: JSX.IntrinsicElements['textarea'] & { max?: number }) {
  const ref = useRef<HTMLTextAreaElement>(null);
  useLayoutEffect(() => fit(ref.current, max), [props.value, max]);
  return <textarea ref={ref} {...props} />;
}

/** Sizes a text box to its text, up to max px. */
export function fit(a: HTMLTextAreaElement | null, max = Infinity) {
  if (!a) return;
  a.style.height = 'auto';
  // scrollHeight leaves out the border, which a border-box height includes:
  // without it the box is 2px short of its text and shows a scroll bar.
  const h = a.scrollHeight + a.offsetHeight - a.clientHeight;
  a.style.height = `${Math.min(h, max)}px`;
  a.style.overflowY = h > max ? 'auto' : 'hidden';
}
