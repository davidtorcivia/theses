// The board page.

import { state, emit } from './state.js';
import { start } from './chrome.js';
import { renderWork } from './workspace.js';
import { renderDrawer } from './drawer.js';
import { openPanel } from './activity.js';
import { beforeRender, afterRender } from './docs.js';

let scrollNotes = location.hash === '#notes';
const tabFromLocation = () => {
  const tab = location.hash.slice(1);
  state.tab = ['links', 'files'].includes(tab) ? tab : 'board';
};

let first = true;
start(() => {
  if (first) { first = false; tabFromLocation(); }
  // The document brackets the rebuild: the whole page is made again from the
  // state, and the block somebody is editing has to survive that with its text,
  // its caret and the version it started from.
  beforeRender();
  renderWork();
  renderDrawer();
  afterRender();
  if (scrollNotes && document.querySelector('.doc-ph')) {
    scrollNotes = false;
    requestAnimationFrame(() => document.querySelector('.doc-ph')?.scrollIntoView());
  }
});

window.addEventListener('hashchange', () => {
  const tab = location.hash.slice(1);
  // Activity is a panel rather than a pane, so it has no tab of its own to
  // land on. The settings page links here and names it in the hash.
  if (tab === 'activity') { openPanel(); return; }
  scrollNotes = tab === 'notes';
  tabFromLocation();
  emit();
});
