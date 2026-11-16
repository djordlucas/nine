/**
 * The UI's sections (adr/web-ui.md §5). On a phone every section is a tab in
 * the bottom bar; on desktop the `foot` ones pin to the bottom of the rail.
 */
export interface Section {
  path: string;
  label: string;
  icon: IconName;
  /** Pinned to the bottom of the desktop rail, above the controls. */
  foot?: boolean;
}

export type IconName =
  | 'inspect'
  | 'context'
  | 'power'
  | 'trash'
  | 'stop'
  | 'play'
  | 'reload'
  | 'download'
  | 'upload'
  | 'filePlus'
  | 'file'
  | 'folder'
  | 'search'
  | 'image'
  | 'plus'
  | 'prev'
  | 'next'
  | 'send'
  | 'dashboard'
  | 'chat'
  | 'process'
  | 'board'
  | 'goal'
  | 'tool'
  | 'inbox'
  | 'journal'
  | 'data'
  | 'system';

export const sections: Section[] = [
  { path: '/conversations', label: 'Agents', icon: 'chat' },
  { path: '/tools', label: 'Tools', icon: 'tool' },
  { path: '/inbox', label: 'Inbox', icon: 'inbox' },
  { path: '/data', label: 'Data', icon: 'data' },
  { path: '/journal', label: 'Journal', icon: 'journal' },
  { path: '/builder', label: 'Builder', icon: 'dashboard' },
  { path: '/system', label: 'System', icon: 'system', foot: true },
];

/** The section a path belongs to: the longest matching prefix. */
export function sectionFor(path: string): Section {
  let best = sections[0];
  for (const s of sections) {
    if (s.path !== '/' && (path === s.path || path.startsWith(s.path + '/'))) best = s;
  }
  return best;
}
