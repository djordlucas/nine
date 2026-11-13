/**
 * The UI's sections (adr/web-ui.md §5). `tab` marks the ones the mobile tab
 * bar holds; the rest are under More.
 */
export interface Section {
  path: string;
  label: string;
  icon: IconName;
  tab: boolean;
  /** Pinned to the bottom of the sidebar, above the controls, not in the list. */
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
  | 'plus'
  | 'prev'
  | 'next'
  | 'dashboard'
  | 'chat'
  | 'process'
  | 'board'
  | 'goal'
  | 'tool'
  | 'inbox'
  | 'journal'
  | 'data'
  | 'system'
  | 'more';

export const sections: Section[] = [
  { path: '/conversations', label: 'Agents', icon: 'chat', tab: true },
  { path: '/tools', label: 'Tools', icon: 'tool', tab: false },
  { path: '/inbox', label: 'Inbox', icon: 'inbox', tab: true },
  { path: '/data', label: 'Data', icon: 'data', tab: false },
  { path: '/journal', label: 'Journal', icon: 'journal', tab: false },
  { path: '/system', label: 'System', icon: 'system', tab: false, foot: true },
];

/** The section a path belongs to: the longest matching prefix. */
export function sectionFor(path: string): Section {
  let best = sections[0];
  for (const s of sections) {
    if (s.path !== '/' && (path === s.path || path.startsWith(s.path + '/'))) best = s;
  }
  return best;
}
