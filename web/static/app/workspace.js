// The workspace: the head with the number, title, statement, status, episode
// and members, the four tabs, and the panes under them.

import { $, el, children, num, initials, say, editable } from './dom.js';
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

const tabsFor = (p) => p.kind === 'show'
  ? [['board', 'Board'], ['files', 'Files']]
  : [['board', 'Board'], ['links', 'Links'], ['files', 'Files']];

// found is the way back to whatever in the work area has the keyboard, written
// down before the rebuild throws it away. Everything here that takes the focus
// and is not a form control carries something to find it again by: the title
// and the statement their ids, a card its number, a column name the column's.
function found(work) {
  const node = document.activeElement;
  if (!node || !work.contains(node)) return '';
  if (node.id === 'wtitle' || node.id === 'wstate') return '#' + node.id;
  if (node.classList.contains('card')) return `#board .card[data-id="${node.dataset.id}"]`;
  const column = node.tagName === 'H3' ? node.closest('.col') : null;
  return column ? `#board .col[data-col="${column.dataset.col}"] h3` : '';
}

export function renderWork() {
  // The head is built again from nothing, so whatever held the keyboard in it
  // is thrown away with it and the focus goes back on the new node at the end.
  // Without this, somebody who had just reached the title would find the
  // keyboard on the body the moment anybody else touched this proposition. A
  // card or a column name that was kept across the render keeps its focus by
  // itself and this puts it where it already is.
  const back = found($('#work'));
  const work = $('#work');
  const p = open();
  if (!p) {
    children(work, [el('p', { class: 'empty', text: nothing() })]);
    return;
  }
  document.title = (p.kind === 'show' ? p.title : `${num(p.number)} ${p.title}`) + ' · THESES';
  children(work, [head(p), pane(p)]);
  if (back) {
    const node = $(back);
    if (node) node.focus();
  }
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
  for (const [id, label] of tabsFor(p)) {
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
  tabs.append(el('a', { class: 'tab', href: p.kind === 'show' ? '/show/settings' : `/p/${p.id}/settings` },
    p.kind === 'show' ? 'Show settings' : 'Settings'));

  return el('div', { class: 'whead' },
    el('div', { class: 'wid' }, p.kind === 'show' ? null : el('span', { id: 'wnum', class: 'mono', text: num(p.number) }), title),
    statement,
    p.kind === 'show' ? null : el('div', { class: 'wmeta' },
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

// editOnClick leans on head() being built again from nothing on every render.
// editable leaves what it wrote in the node, and on Escape or an unchanged
// commit that is not what the state holds, so the node it put it in has to be
// a new one. Anything that starts keeping the head between renders has to throw
// the node away when an edit ends, the way the board's column heading and the
// rail's row do.
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

// The board pane and the board inside it are made once and kept. A card is
// carried by the pointer, and a pane rebuilt around one would take it out of
// the page under the finger holding it.
let boardPane = null;

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

  if (!boardPane) {
    boardPane = el('section', { class: 'pane', id: 'pane-board' },
      el('div', { class: 'ph' }), el('div', { id: 'board' }));
  }
  const ph = $('.ph', boardPane);
  const board = $('#board', boardPane);
  children(ph, [
    el('h2', { text: 'Board' }),
    el('span', { id: 'bsum', class: 'mono', text: boardSummary() }),
    filters,
    // Both are drawn and the stylesheet shows the one that is true for the
    // reader, because a phone is told to press rather than to drag.
    el('span', { class: 'hint mono', text: 'Drag cards between columns. Click a name to rename a column. + on a card assigns.' }),
    el('span', { class: 'hint press mono', text: 'Press and hold a card to move it.' }),
  ]);
  renderBoard(board);
  children(boardPane, [ph, board, renderDocument()]);
  return boardPane;
}
