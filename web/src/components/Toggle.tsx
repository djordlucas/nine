import type { ComponentChildren } from 'preact';
import css from './Toggle.module.css';

/**
 * An on/off choice: a small square, filled when on, beside its label. It is
 * the chat's T button with words, in place of the browser's checkbox.
 */
export function Toggle(props: { on: boolean; onChange: (on: boolean) => void; disabled?: boolean; title?: string; children: ComponentChildren }) {
  return (
    <button
      type="button"
      class={css.toggle}
      aria-pressed={props.on}
      disabled={props.disabled}
      title={props.title}
      onClick={() => props.onChange(!props.on)}
    >
      <span class={css.box} aria-hidden="true">
        {props.on ? '✓' : ''}
      </span>
      <span>{props.children}</span>
    </button>
  );
}
