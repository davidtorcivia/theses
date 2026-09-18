// The workspace: the head with the number, title, statement, status, episode
// and members, the four tabs, and the panes under them.

import { $, el, clear, num, initials, say, editable } from './dom.js';
import { state, user, open, emit, hold, canEdit, archived } from './state.js';
import { send } from './net.js';
import { renderBoard, boardSummary } from './board.js';
import { renderDocument } from './docs.js';
import { renderLinks } from './links.js';
import { renderFiles } from './files.js';
import { openPanel, outstanding } from './activity.js';
import { activate } from './keys.js';

// Activity is a panel rather than a pane, so it has no tab of its own to land
// on. The settings page's Activity tab links here and names it in the hash.
if (location.hash === '#activity') openPanel();

const TABS = [['board', 'Board'], ['links', 'Links'], ['files', 'Files']];

export function renderWork() {
  const work = clear($('#work'));
  const p = open();
  if (!p) {
    work.append(el('p', { class: 'empty', text: nothing() }));
    return;
  }
  document.title = `${num(p.number)} ${p.title} · THESES`;
  work.append(head(p));
  work.append(pane(p));
}

// nothing is the line an empty work area carries. A proposition is per
// membership, so an account that is on none opens a workspace with nothing in
// it and used to be told only that there was nothing. Now it is told why.
function nothing() {
  if (state.fromCache) {
    return 'Nothing of this proposition is on this device. It will be here when the connection is back.';
  }
  if (!state.props.length && user(state.me).role !== 'owner') {
    return "You are not on any proposition yet. An owner adds you from a proposition's settings.";
  }
  return 'Nothing here yet.';
}

function head(p) {
  const title = el('h1', { id: 'wtitle', text: p.title, spellcheck: 'false' });
  const statement = el('p', { id: 'wstate', text: p.statement, spellcheck: 'false' });
  if (canEdit()) {
    editOnClick(title, () => p.title, (value) =>
      send('proposition.edit', { proposition: p.id, title: value, statement: p.statement, blurb: p.blurb }));
    editOnClick(statement, () => p.statement, (value) =>
      send('proposition.edit', { proposition: p.id, title: p.title, statement: value, blurb: p.blurb }));
  }

  const members = el('span', { id: 'wmembers', class: 'members' });
  for (const id of p.members || []) members.append(initials(user(id), on(id) ? 'on' : ''));

  const tabs = el('nav', { class: 'tabs' });
  for (const [id, label] of TABS) {
    tabs.append(el('a', {
      class: 'tab' + (state.tab === id ? ' on' : ''), 'data-tab': id, href: '#' + id,
      onclick: (e) => { e.preventDefault(); state.tab = id; location.hash = id; emit(); },
    }, label));
  }
  // Activity is a panel rather than a pane: it opens beside the work instead of
  // taking its place, and it carries the count of what has not gone up yet.
  const held = outstanding();
  tabs.append(el('button', {
    class: 'tab' + (state.panel ? ' on' : ''), id: 'activitytab', type: 'button',
    onclick: openPanel,
  }, 'Activity', held ? el('i', { text: ' ' + held }) : null));
  tabs.append(el('a', { class: 'tab', href: `/p/${p.id}/settings` }, 'Settings'));

  return el('div', { class: 'whead' },
    el('div', { class: 'wid' }, el('span', { id: 'wnum', class: 'mono', text: num(p.number) }), title),
    statement,
    el('div', { class: 'wmeta' },
      el('span', { id: 'wstatus', class: 'status', 'data-s': p.status, text: p.status }),
      archived() && el('span', { class: 'mono', text: 'Archived · read only' }),
      el('span', { id: 'wep', class: 'mono', text: schedule(p) }),
      members),
    tabs);
}

const on = (id) => state.presence.some((x) => x.id === id);

function schedule(p) {
  if (!p.episode) return 'Not scheduled';
  return 'Episode ' + p.episode + (p.target_date ? ' · target ' + p.target_date : '');
}

function editOnClick(node, read, save) {
  const edit = () => {
    if (node.isContentEditable) return;
    hold(true);
    editable(node, read(), (value) => {
      hold(false);
      if (value === null || value === read()) { emit(); return; }
      save(value).catch((err) => { say(err.message); emit(); });
    });
  };
  node.addEventListener('click', edit);
  activate(node, edit);
}

function pane(p) {
  if (state.tab === 'links') {
    const links = el('section', { class: 'pane', id: 'pane-links' });
    renderLinks(links);
    return links;
  }
  if (state.tab === 'files') {
    const files = el('section', { class: 'pane', id: 'pane-files' });
    renderFiles(files);
    return files;
  }

  const filters = el('div', { id: 'bfilter', class: 'facets' });
  for (const [id, label] of [['all', 'All'], ['mine', 'Mine'], ['open', 'Open']]) {
    filters.append(el('button', {
      type: 'button', 'data-f': id, text: label,
      class: state.boardFilter === id ? 'on' : '',
      onclick: () => { state.boardFilter = id; emit(); },
    }));
  }

  const board = el('div', { id: 'board' });
  renderBoard(board);

  return el('section', { class: 'pane', id: 'pane-board' },
    el('div', { class: 'ph' },
      el('h2', { text: 'Board' }),
      el('span', { id: 'bsum', class: 'mono', text: boardSummary() }),
      filters,
      el('span', { class: 'hint mono', text: 'Drag cards between columns. Click a name to rename a column. + on a card assigns.' })),
    board,
    renderDocument());
}
