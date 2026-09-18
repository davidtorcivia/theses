// The workspace: the head with the number, title, statement, status, episode
// and members, the four tabs, and the panes under them.

import { $, el, clear, num, initials, say, editable } from './dom.js';
import { state, user, open, emit, hold } from './state.js';
import { send } from './net.js';
import { renderBoard, boardSummary } from './board.js';

const TABS = [['board', 'Board'], ['links', 'Links'], ['files', 'Files']];

export function renderWork() {
  const work = clear($('#work'));
  const p = open();
  if (!p) {
    work.append(el('p', { class: 'empty', text: 'Nothing here yet.' }));
    return;
  }
  document.title = `${num(p.number)} ${p.title} · THESES`;
  work.append(head(p));
  work.append(pane(p));
}

function head(p) {
  const title = el('h1', { id: 'wtitle', text: p.title, spellcheck: 'false' });
  const statement = el('p', { id: 'wstate', text: p.statement, spellcheck: 'false' });
  if (state.can.edit) {
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
  tabs.append(el('a', { class: 'tab', href: `/p/${p.id}/settings` }, 'Settings'));

  return el('div', { class: 'whead' },
    el('div', { class: 'wid' }, el('span', { id: 'wnum', class: 'mono', text: num(p.number) }), title),
    statement,
    el('div', { class: 'wmeta' },
      el('span', { id: 'wstatus', class: 'status', 'data-s': p.status, text: p.status }),
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
  node.addEventListener('click', () => {
    if (node.isContentEditable) return;
    hold(true);
    editable(node, read(), (value) => {
      hold(false);
      if (value === null || value === read()) { emit(); return; }
      save(value).catch((err) => { say(err.message); emit(); });
    });
  });
}

function pane(p) {
  if (state.tab === 'links') {
    return el('section', { class: 'pane', id: 'pane-links' },
      el('div', { class: 'ph' }, el('h2', { text: 'Links' })),
      el('p', { class: 'empty', text: 'Links arrive in step four, with the metadata fetch and the citations.' }));
  }
  if (state.tab === 'files') {
    return el('section', { class: 'pane', id: 'pane-files' },
      el('div', { class: 'ph' }, el('h2', { text: 'Files' })),
      el('p', { class: 'empty', text: 'Files arrive in step four, with the presigned uploads and the versions.' }));
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
    el('div', { class: 'ph doc-ph' },
      el('div', { id: 'doctabs', class: 'doctabs' }, el('button', { class: 'dtab on', type: 'button', text: 'Research' })),
      el('span', { id: 'dsum', class: 'mono', text: 'The document lands in step three.' })),
    el('div', { id: 'docwrap' }, el('div', { id: 'doc' },
      el('p', { class: 'empty', text: 'The shared document under the board arrives in step three, with its blocks, its merge and its markdown mirror.' }))));
}
