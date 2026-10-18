import { useEffect, useRef } from 'preact/hooks';
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
                onClose();
                ask.onConfirm();
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
