import { useEffect, useRef, useState } from 'preact/hooks';
import styles from './Confirm.module.css';

export type Ask = {
  title: string;
  message: string;
  /** The confirming button's label, which names the action. */
  confirm: string;
  /** The action destroys something: its button is red. */
  danger?: boolean;
  onConfirm: () => void;
};

/**
 * A question before an action, in the UI's own style instead of the browser's
 * confirm(). A modal <dialog>: focus stays inside it, Escape or a click on the
 * backdrop cancels, and the safe choice, Cancel, has the focus first.
 */
export function Confirm({ ask, onClose }: { ask: Ask | null; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const cancel = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    const d = dialog.current;
    if (!d) return;
    if (ask && !d.open) {
      d.showModal();
      cancel.current?.focus();
    } else if (!ask && d.open) d.close();
  }, [ask]);

  return (
    <dialog
      ref={dialog}
      class={styles.dialog}
      aria-labelledby="confirm-title"
      aria-describedby="confirm-message"
      onClose={onClose}
      onClick={(e) => {
        // A click outside the box lands on the dialog element itself.
        if (e.target === dialog.current) onClose();
      }}
    >
      {ask && (
        <div class={styles.box}>
          <h2 id="confirm-title">{ask.title}</h2>
          <p id="confirm-message">{ask.message}</p>
          <div class={styles.actions}>
            <button ref={cancel} type="button" class={styles.cancel} onClick={onClose}>
              Cancel
            </button>
            <button
              type="button"
              class={`button ${ask.danger ? styles.danger : ''}`}
              onClick={() => {
                ask.onConfirm();
                onClose();
              }}
            >
              {ask.confirm}
            </button>
          </div>
        </div>
      )}
    </dialog>
  );
}

/** A question for ask(): the dialog's title, message and confirming label. */
export type Question = Omit<Ask, 'onConfirm'>;

type Pending = Question & { resolve: (ok: boolean) => void };

let show: ((p: Pending) => void) | null = null;

/**
 * Asks before an action, in the dialog ConfirmHost holds, and resolves to
 * whether the operator confirmed. Without a host, it falls back to the
 * browser's confirm().
 */
export function ask(q: Question): Promise<boolean> {
  return new Promise((resolve) => {
    if (show) show({ ...q, resolve });
    else resolve(window.confirm(q.message));
  });
}

/** The one dialog every ask() uses, mounted once in the app. */
export function ConfirmHost() {
  const [pending, setPending] = useState<Pending | null>(null);
  useEffect(() => {
    show = (p) =>
      setPending((was) => {
        // A question asked over another one cancels the first.
        was?.resolve(false);
        return p;
      });
    return () => {
      show = null;
    };
  }, []);
  return (
    <Confirm
      ask={pending && { ...pending, onConfirm: () => pending.resolve(true) }}
      onClose={() => {
        // Settled already when confirmed: a promise resolves once.
        pending?.resolve(false);
        setPending(null);
      }}
    />
  );
}
