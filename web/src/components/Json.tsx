import { asJson, jsonTokens, type JsonToken } from '../json';
import { tomlTokens } from '../toml';
import { pretty } from '../format';
import styles from './Json.module.css';

/**
 * A value in a <pre>: JSON with its keys, strings, numbers and literals
 * coloured, anything else as plain text.
 */
export function Json({ value, class: cls }: { value: unknown; class?: string }) {
  const text = asJson(value);
  if (text === null) return <pre class={cls}>{pretty(value)}</pre>;
  return <Tokens tokens={jsonTokens(text)} class={cls} />;
}

/** TOML in a <pre>, its keys and values coloured as JSON's are. */
export function Toml({ text, class: cls }: { text: string; class?: string }) {
  return <Tokens tokens={tomlTokens(text)} class={cls} />;
}

function Tokens({ tokens, class: cls }: { tokens: JsonToken[]; class?: string }) {
  return (
    <pre class={cls}>
      {tokens.map((t, i) => (t.kind === 'space' ? t.text : <span key={i} class={styles[t.kind]}>{t.text}</span>))}
    </pre>
  );
}
