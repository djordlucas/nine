import type { ComponentChildren } from 'preact';
import { createContext } from 'preact';
import { useContext, useState } from 'preact/hooks';
import css from './Split.module.css';

/**
 * A list beside its open item (adr/web-ui.md §5). On a desktop the list folds
 * away to a thin strip, remembered per browser under name; on a phone the
 * list, then the item in its place, so nothing folds.
 */
export function Split({ name, noun, open, children }: { name: string; noun: string; open: boolean; children: ComponentChildren }) {
  const [folded, setFolded] = useState(() => readFolded(name));
  const fold = (f: boolean) => {
    setFolded(f);
    writeFolded(name, f);
  };
  return (
    <Fold.Provider value={{ noun, fold: () => fold(true) }}>
      <div class={`${css.split} ${open ? css.open : ''} ${folded ? css.collapsed : ''}`}>
        {folded && (
          <div class={css.strip}>
            <FoldButton expanded={false} label={`Show ${noun}`} onClick={() => fold(false)} />
          </div>
        )}
        {children}
      </div>
    </Fold.Provider>
  );
}

/** The list's first row: its own controls, then the button that folds it. */
export function ListHead({ children }: { children?: ComponentChildren }) {
  const f = useContext(Fold);
  return (
    <div class={css.top}>
      {children}
      {f && <FoldButton expanded label={`Hide ${f.noun}`} onClick={f.fold} />}
    </div>
  );
}

const Fold = createContext<{ noun: string; fold: () => void } | null>(null);

function FoldButton({ expanded, label, onClick }: { expanded: boolean; label: string; onClick: () => void }) {
  return (
    <button type="button" class={`button ${css.fold}`} aria-label={label} title={label} aria-expanded={expanded} onClick={onClick}>
      {expanded ? '‹' : '›'}
    </button>
  );
}

// Storage can be missing or refuse, which reads as open.
function readFolded(name: string): boolean {
  try {
    return localStorage.getItem(`nine.${name}.collapsed`) === '1';
  } catch {
    return false;
  }
}

function writeFolded(name: string, f: boolean) {
  try {
    if (f) localStorage.setItem(`nine.${name}.collapsed`, '1');
    else localStorage.removeItem(`nine.${name}.collapsed`);
  } catch {
    // Not remembered; the list still folds.
  }
}
