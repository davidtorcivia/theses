// The board page.

import { state, emit, material, unresolvedCard } from './state.js';
import { start } from './chrome.js';
import { renderWork } from './workspace.js';
import { renderDrawer, openCard } from './drawer.js';
import { openPanel } from './activity.js';
import { beforeRender, afterRender } from './docs.js';

import { openFile } from './files.js';
import { openLink } from './links.js';
import { parseTarget } from './anchors.js';
import { say } from './dom.js';

let routed = '';
let routing = 0;
const routeKey = () => state.open + ':' + location.hash + ((/^#(file|link)-/.test(location.hash)) ? ':' + state.loaded : '');
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
  const route = routeKey();
  if (state.open && route !== routed) {
    routed = route;
    queueMicrotask(routeTarget);
  }
  if (scrollNotes && document.querySelector('.doc-ph')) {
    scrollNotes = false;
    requestAnimationFrame(() => document.querySelector('.doc-ph')?.scrollIntoView());
  }
});

window.addEventListener('hashchange', () => {
  routed = '';
  const tab = location.hash.slice(1);
  // Activity is a panel rather than a pane, so it has no tab of its own to
  // land on. The settings page links here and names it in the hash.
  scrollNotes = tab === 'notes';
  tabFromLocation();
  emit();
});

async function routeTarget() {
  const generation = ++routing;
  const target = parseTarget(location.hash);
  if (unresolvedCard(state.openCard) && (target?.kind !== 'card' || target.id !== state.openCard)) {
    history.pushState(null, '', '#card-' + state.openCard);
    routed = routeKey();
    state.tab = 'board';
    emit();
    say('Resolve this card’s conflict before navigating away.');
    return;
  }
  state.panel = false;
  if (location.hash === '#activity') { openPanel(); return; }
  if (!target) { state.openCard = state.openFile = state.openLink = null; emit(); return; }
  const { kind, id } = target;
  if (kind === 'file' || kind === 'link') {
    state.tab = kind === 'file' ? 'files' : 'links';
    try { await material(); } catch { say('Could not load this item. Retry when connected.'); return; }
    if (generation !== routing || parseTarget(location.hash)?.id !== id || parseTarget(location.hash)?.kind !== kind) return;
    if (state.loaded !== state.open && !state.fromCache) { say('This item could not be loaded. Retry when connected.'); return; }
    const rows = kind === 'file' ? state.files : state.links;
    if (rows.some((row) => row.id === id)) { (kind === 'file' ? openFile : openLink)(id); return; }
  } else if (kind === 'card' || kind === 'comment') {
    const card = kind === 'card' ? state.cards.get(id)
      : [...state.cards.values()].find((c) => (c.comments || []).some((comment) => comment.id === id));
    state.tab = 'board';
    if (card) {
      openCard(card.id, false);
      if (kind === 'comment') requestAnimationFrame(() => {
        const node = document.querySelector(`[data-comment="${id}"]`);
        node?.scrollIntoView({ block: 'center' });
        node?.focus({ preventScroll: true });
      });
      return;
    }
  } else {
    const doc = state.documents.find((d) => kind === 'document' ? d.id === id
      : (d.blocks || []).some((b) => b.id === id && !b.deleted_at));
    if (doc) {
      state.openCard = state.openFile = state.openLink = null;
      state.document = doc.id;
      state.docSource = false;
      state.tab = 'board';
      emit();
      requestAnimationFrame(() => {
        const node = document.querySelector(kind === 'block' ? `#doc .blk[data-b="${id}"]` : '.doc-ph');
        node?.scrollIntoView({ block: 'center' });
        if (node) { if (kind === 'document') node.tabIndex = -1; node.focus({ preventScroll: true }); }
      });
      return;
    }
  }
  state.openCard = state.openFile = state.openLink = null;
  emit();
  say('This item is unavailable or has been deleted.');
}
