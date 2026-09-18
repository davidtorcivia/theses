// The board page.

import { state, emit } from './state.js';
import { start } from './chrome.js';
import { renderWork } from './workspace.js';
import { renderDrawer } from './drawer.js';

const startTab = location.hash.slice(1);
if (['links', 'files'].includes(startTab)) state.tab = startTab;

start(() => {
  renderWork();
  renderDrawer();
});

window.addEventListener('hashchange', () => {
  const tab = location.hash.slice(1);
  state.tab = ['links', 'files'].includes(tab) ? tab : 'board';
  emit();
});
