import type { ComponentChildren } from 'preact';
import detail from './Detail.module.css';

/** A card that says what a pane is for when nothing in its list is selected,
 * so a section's page is never a bare pane. */
export function Blank({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <div class={detail.title}>
          <h2>{title}</h2>
        </div>
      </header>
      <p class="muted">{children}</p>
    </article>
  );
}
