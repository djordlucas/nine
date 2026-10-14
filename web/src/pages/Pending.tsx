import { useLocation } from 'preact-iso';
import { sectionFor } from '../sections';

/** A section whose screens are not built yet (adr/web-ui.md §9). */
export function Pending() {
  const { path } = useLocation();
  return (
    <div class="card">
      <p class="muted">The {sectionFor(path).label} section is not built yet.</p>
    </div>
  );
}

export function NotFound() {
  return (
    <div class="card">
      <p>
        Nothing here. <a href="/">Back to the dashboard</a>
      </p>
    </div>
  );
}
