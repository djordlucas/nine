import type { ComponentChildren } from 'preact';
import styles from './Blank.module.css';

/** A card that says what a pane is for when nothing in its list is selected,
 * so a section's page is never a bare pane: centered, not a lone paragraph
 * in a corner. An optional action is the pane's call to action. */
export function Blank({
  title,
  children,
  action,
}: {
  title: string;
  children: ComponentChildren;
  action?: { label: string; onClick: () => void };
}) {
  return (
    <article class={`card ${styles.blank}`}>
      <h2>{title}</h2>
      <p class="muted">{children}</p>
      {action && (
        <button type="button" class="button" onClick={action.onClick}>
          {action.label}
        </button>
      )}
    </article>
  );
}
