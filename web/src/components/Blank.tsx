import type { ComponentChildren } from 'preact';
import styles from './Blank.module.css';

/** A card that says what a pane is for when nothing in its list is selected,
 * so a section's page is never a bare pane: centered, not a lone paragraph
 * in a corner. */
export function Blank({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <article class={`card ${styles.blank}`}>
      <h2>{title}</h2>
      <p class="muted">{children}</p>
    </article>
  );
}
