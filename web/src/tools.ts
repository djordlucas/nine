import type { Schemas } from './api/client';

export type Tool = Schemas['ToolInfo'];

/** The kinds of tool, in the order the roster groups them. */
export const kindOrder = ['generated', 'sandboxed', 'plugin', 'core'] as const;

export const kindLabels: Record<string, string> = {
  generated: 'Written by Nine',
  sandboxed: 'Sandboxed',
  plugin: 'Plugins',
  core: 'Built in',
};

/**
 * Tools grouped by kind, in kindOrder, each group by name, keeping only those
 * whose name, description or plugin contains filter (any case).
 */
export function groupTools(tools: readonly Tool[], filter = ''): [string, Tool[]][] {
  const f = filter.trim().toLowerCase();
  const groups = new Map<string, Tool[]>();
  for (const t of tools) {
    const hay = `${t.name ?? ''} ${t.description ?? ''} ${t.plugin ?? ''}`.toLowerCase();
    if (f && !hay.includes(f)) continue;
    const kind = t.kind ?? 'plugin';
    const list = groups.get(kind) ?? [];
    list.push(t);
    groups.set(kind, list);
  }
  const kinds = [...groups.keys()].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b));
  return kinds.map((k) => [k, groups.get(k)!.sort((a, b) => (a.name ?? '').localeCompare(b.name ?? ''))]);
}

function rank(kind: string): number {
  const i = (kindOrder as readonly string[]).indexOf(kind);
  return i < 0 ? kindOrder.length : i;
}

/** Tool arguments typed as JSON: the object, or why it is not one. */
export function parseArgs(text: string): { args: Record<string, unknown> } | { error: string } {
  const t = text.trim();
  if (t === '') return { args: {} };
  let v: unknown;
  try {
    v = JSON.parse(t);
  } catch (e) {
    return { error: `Not JSON: ${e instanceof Error ? e.message : String(e)}` };
  }
  if (v === null || typeof v !== 'object' || Array.isArray(v)) return { error: 'The arguments must be a JSON object.' };
  return { args: v as Record<string, unknown> };
}

/**
 * A starting point for a tool's arguments: each required property of its
 * schema with an empty value of its type.
 */
export function argsTemplate(schema: unknown): string {
  const s = schema as { properties?: Record<string, { type?: string }>; required?: string[] } | undefined;
  const out: Record<string, unknown> = {};
  for (const name of s?.required ?? []) {
    const type = s?.properties?.[name]?.type;
    out[name] = type === 'number' || type === 'integer' ? 0 : type === 'boolean' ? false : type === 'array' ? [] : type === 'object' ? {} : '';
  }
  return JSON.stringify(out, null, 2);
}
