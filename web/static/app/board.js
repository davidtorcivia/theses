import { matchesCard } from './filters.js';
// The board: columns that wrap, cards that drag between and within them, the
// inline form that assigns by account name, and the All, Mine and Open filter.

import { $, el, children, initials, say, editable, handles, stripHandles, inline } from './dom.js';
import { state, user, byHandle, proposition, emit, hold, columnCards, canEdit } from './state.js';
import { send } from './net.js';
import { openPicker, closePicker, mentionable } from './picker.js';
import { openCard } from './drawer.js';
import { activate } from './keys.js';
import { movable, carrying } from './drag.js';
import { propositionToken, propositionLabel, referenceIDs, referenceKey } from './references.js';

// A due date is an ISO calendar day, so late compares two of those and never
// two instants: a card due today is not late at any hour of it. The day it is
// held against is the workspace's, which is the show's day rather than the day
// wherever the person reading the board happens to be sitting.
const ISO = /^\d{4}-\d{2}-\d{2}$/;

// A card that is done is never late, whatever day is on it: the work it was
// asking for has happened.
const late = (card) => !card.done_at && ISO.test(card.due_date || '') && card.due_date < today();

// The formatter is kept rather than built per card, because a full board asks
// this once per card per render. An empty or unknown zone throws on the way in
// and leaves the laptop's own day, which is the best guess there is.
let zone = { name: undefined, format: null };

function today() {
  const now = new Date();
  if (state.timezone !== zone.name) {
    zone = { name: state.timezone, format: null };
    try {
      zone.format = new Intl.DateTimeFormat('en-CA',
        { timeZone: state.timezone, year: 'numeric', month: '2-digit', day: '2-digit' });
    } catch {
      // Nothing usable in the setting. The line below answers instead.
    }
  }
  const there = zone.format ? zone.format.format(now) : '';
  if (ISO.test(there)) return there;
  const pad = (n) => String(n).padStart(2, '0');
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

function visible(card) {
  return matchesCard(card, { ...state.boardOptions, status: state.boardFilter }, state.me, today());
}

function meta(card) {
  const bits = [];
  if (card.due_date) bits.push(el('span', { class: 'due' + (late(card) ? ' late' : ''), text: card.due_date }));
  if (card.question) bits.push(el('span', { class: 'q', text: card.question }));
  for (const id of referenceIDs(card.title)) {
    const p = proposition(id);
    if (p) bits.push(el('span', { class: 'status', text: p.archived_at ? 'Archived' : p.status }));
  }
  const list = card.checklist || [];
  if (list.length) bits.push(el('span', { class: 'chk', text: list.filter((i) => i.done).length + '/' + list.length }));
  const notes = (card.comments || []).length;
  if (notes) bits.push(el('span', { class: 'cmt', text: String(notes) }));
  return bits;
}

// drawn is the node each card was last rendered as, with the key it was built
// from. The whole page is made again from the state on every applied event, and
// with somebody saving a document every second, rebuilding cards nobody touched
// would throw away the finger carrying one for nothing.
const drawn = new Map();

// The key is everything a card's appearance and its listeners are built from.
// late is in it rather than only the date, because which day it is decides it
// and the day turns without any event: the first render after midnight draws
// the card again. Each assignee carries what the square of initials is drawn
// from, so somebody renaming themselves redraws the cards they are on.
function cardKey(card) {
  const list = card.checklist || [];
  return JSON.stringify([
    canEdit(), (card.assignees || []).includes(state.me), late(card),
    referenceKey(card.title, proposition), card.title, card.done_at, card.due_date, card.question,
    list.filter((i) => i.done).length, list.length, (card.comments || []).length,
    (card.assignees || []).map((id) => {
      const person = user(id);
      return [id, person.initials, person.colour, person.name];
    }),
  ]);
}

function cardNode(card) {
  const key = cardKey(card);
  const was = drawn.get(card.id);
  if (was && was.key === key) return was.node;

  // A node that is kept holds the closures it was built with, and the row it
  // was built from is replaced by every event that touches the card. So the
  // listeners read the card as it is now rather than the copy they closed over.
  const id = card.id;
  const row = () => state.cards.get(id) || card;
  const mine = (card.assignees || []).includes(state.me);
  const who = el('div', { class: 'cw' });
  for (const person of card.assignees || []) who.append(initials(user(person)));
  if (canEdit()) {
    who.append(el('button', {
      class: 'asg', type: 'button', title: 'Assign someone', text: '+',
      onclick: (e) => { e.stopPropagation(); assign(row(), e.currentTarget); },
    }));
  }

  const tick = el('button', {
    class: 'tick', type: 'button', text: card.done_at ? 'Done' : 'Mark done',
    onclick: (e) => { e.stopPropagation(); toggleDone(row()); },
  });
  const node = el('article', {
    class: 'card' + (card.done_at ? ' done' : '') + (mine ? ' mine' : ''),
    'data-id': id,
  }, who, el('div', { class: 'cb' },
    el('div', { class: 'ct' }, inline(card.title, byHandle, proposition)),
    el('div', { class: 'cm' }, meta(card), canEdit() && tick)));

  node.addEventListener('click', (e) => {
    if (e.target.closest('a')) return;
    // The pointer that has just carried a card ends in a click as well, and
    // that one finishes the drag rather than asking to read the card.
    if (carrying()) return;
    openCard(id);
  });
  activate(node, () => openCard(id));
  // Cards are carried by the pointer, because a phone fires no drag event from
  // a finger. A card let go anywhere but over a column moves nothing.
  if (canEdit()) {
    movable(node, {
      zone: '.col', list: (col) => $('.cards', col), over: 'over',
      drop: (col) => {
        const previous = node.previousElementSibling;
        send('card.move', {
          card: id,
          column: Number(col.dataset.col),
          after: previous ? Number(previous.dataset.id) : 0,
        }).catch((err) => { say(err.message); emit(); });
      },
    });
  }
  drawn.set(id, { key, node });
  return node;
}

function assign(card, anchor) {
  openPicker(anchor, card.assignees || [], (person, on) => {
    send(on ? 'card.assign' : 'card.unassign', { card: card.id, user: person.id })
      .catch((err) => say(err.message));
  });
}

export function toggleDone(card) {
  send('card.done', { card: card.id, done: !card.done_at }).catch((err) => say(err.message));
}

// A column's section and the container its cards sit in are made once and kept
// for as long as the column exists, whatever happens to the column itself. The
// count in the header changes every time a card anywhere in it is done, and a
// header that took the section with it would detach every card under it.
const cols = new Map();

function columnNode(column, all, nodes, keep) {
  let col = cols.get(column.id);
  if (!col) {
    col = {
      key: null, head: null, add: null,
      cards: el('div', { class: 'cards' }),
      section: el('section', { class: 'col', 'data-col': column.id }),
    };
    cols.set(column.id, col);
  }

  const done = all.filter((c) => c.done_at).length;
  const count = all.length ? `${done}/${all.length}` : '—';
  const key = JSON.stringify([canEdit(), count, column.name]);
  if (col.key !== key) {
    col.key = key;
    col.head = header(column, count);
    col.add = canEdit() && el('div', { class: 'board-add' }, el('button', {
      class: 'add mono', type: 'button', text: '+ Card',
      onclick: () => inlineAdd(column.id, col.cards),
    }), proposition(state.open)?.kind === 'show' && el('button', {
      class: 'add mono', type: 'button', text: '+ Proposition',
      onclick: () => addPropositionCard(column.id),
    }));
  }

  children(col.cards, nodes, keep);
  children(col.section, [col.head, col.cards, col.add]);
  return col.section;
}

function header(column, count) {
  const name = el('h3', { text: column.name, spellcheck: 'false' });
  if (canEdit()) {
    const rename = (e) => {
      e.stopPropagation();
      if (name.isContentEditable) return;
      hold(true);
      const was = column.name;
      editable(name, was, (value) => {
        hold(false);
        // The heading holds whatever the editor left in it, which on Escape or
        // on a name cleared to nothing is not the name at all. A header whose
        // key has not moved would be handed straight back with it, so the key
        // is dropped whichever way the edit ended, a refusal included: one
        // refused in the same frame as its own guess is a single render with
        // the key where it started. Only the header is built again; the section
        // and the cards in it are the same nodes either way.
        const col = cols.get(column.id);
        if (col) col.key = null;
        if (!value || value === was) { emit(); return; }
        send('column.rename', { column: column.id, title: value })
          .catch((err) => { say(err.message); emit(); });
      });
    };
    name.addEventListener('click', rename);
    activate(name, rename);
  }
  return el('header', {}, name, el('span', { class: 'mono cnt', text: count }));
}

// inlineAdd is the one place a card is written straight onto the board. An
// account name in the line puts that person on the card and comes out of it.
function inlineAdd(column, cards) {
  const field = el('textarea', { rows: '1', placeholder: 'What needs doing? @ people or propositions.' });
  const form = el('div', { class: 'newcard' }, field);
  cards.append(form);
  hold(true);
  field.focus();
  mentionable(field);

  let committed = false;
  const commit = () => {
    if (committed) return;
    committed = true;
    hold(false);
    const line = field.value.trim();
    form.remove();
    if (!line) { emit(); return; }
    const assignees = handles(line).map((h) => state.byHandle.get(h)).filter(Boolean).map((u) => u.id);
    send('card.create', {
      column,
      title: stripHandles(line) || line,
      assignees: assignees.length ? assignees : [state.me],
    }).catch((err) => { say(err.message); emit(); });
  };
  field.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !$('#picker')) { e.preventDefault(); commit(); }
    if (e.key === 'Escape') { committed = true; hold(false); form.remove(); closePicker(); emit(); }
  });
  field.addEventListener('blur', () => setTimeout(commit, 150));
}

export function renderBoard(into) {
  into.classList.toggle('list-view', state.boardOptions.view === 'list');
  // Every card's node is asked for before any column is swept, because a card
  // that moved to another column is still wanted: taking it out of the column
  // it was in before its new one has claimed it would drop it out of the page,
  // and with it the finger carrying it.
  const lists = state.columns.map((column) => {
    const all = columnCards(column.id);
    return { column, all, nodes: all.filter(visible).map(cardNode) };
  });
  const keep = new Set(lists.flatMap((list) => list.nodes));
  const sections = lists.map((list) => columnNode(list.column, list.all, list.nodes, keep));
  children(into, [sections, canEdit() && addColumn()]);

  // What no longer exists takes its node with it. A card the filter is hiding
  // is still a card and keeps its own, so that turning the filter back on costs
  // nothing.
  for (const id of drawn.keys()) if (!state.cards.has(id)) drawn.delete(id);
  const live = new Set(state.columns.map((column) => column.id));
  for (const id of cols.keys()) if (!live.has(id)) cols.delete(id);
}

// The one button at the end of the board. It reads the open proposition when it
// is pressed rather than when it was made, so one node does for the page.
let addcol = null;

function addColumn() {
  return addcol ||= el('button', {
    class: 'addcol mono', id: 'addcol', type: 'button', text: '+ Column',
    onclick: () => send('column.create', { proposition: state.open, title: 'New column' })
      .catch((err) => say(err.message)),
  });
}

export function boardSummary() {
  const cards = [...state.cards.values()];
  return `${cards.filter(visible).length} shown · ${cards.filter((c) => c.done_at).length} of ${cards.length} done`;
}

function addPropositionCard(column) {
  const options = state.props.filter((p) => p.kind !== 'show' && !p.archived_at);
  const select = el('select', { 'aria-label': 'Proposition' }, options.map((p) =>
    el('option', { value: p.id, text: propositionLabel(p) })));
  const cancel = el('button', { type: 'button', class: 'lnk', text: 'Cancel' });
  const submit = el('button', { type: 'submit', class: 'save', text: 'Add to board', disabled: !options.length });
  const form = el('form', {}, el('h3', { id: 'track-proposition-title', text: 'Track a proposition' }),
    el('p', { text: options.length ? 'Its title and status stay linked to the proposition.' : 'Create a proposition first.' }),
    select, el('div', { class: 'acts' }, cancel, submit));
  const dialog = el('dialog', { 'aria-labelledby': 'track-proposition-title' }, form);
  const back = document.activeElement;
  cancel.onclick = () => dialog.close();
  dialog.addEventListener('close', () => { dialog.remove(); if (back?.isConnected) back.focus(); });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    submit.disabled = true;
    try {
      await send('card.create', { column, title: propositionToken(Number(select.value)), assignees: [] });
      dialog.close();
    } catch (err) { say(err.message); submit.disabled = false; }
  });
  document.body.append(dialog);
  dialog.showModal();
}
