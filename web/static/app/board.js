// The board: columns that wrap, cards that drag between and within them, the
// inline form that assigns by account name, and the All, Mine and Open filter.

import { $, $$, el, clear, initials, say, editable, handles, stripHandles } from './dom.js';
import { state, user, emit, hold, columnCards, canEdit } from './state.js';
import { send } from './net.js';
import { openPicker, closePicker, mentionable } from './picker.js';
import { openCard } from './drawer.js';
import { activate } from './keys.js';

// A due date is an ISO calendar day, so late compares two of those and never
// two instants: a card due today is not late at any hour of it. The day it is
// held against is the workspace's, which is the show's day rather than the day
// wherever the person reading the board happens to be sitting.
const ISO = /^\d{4}-\d{2}-\d{2}$/;

const late = (due) => ISO.test(due || '') && due < today();

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
  if (state.boardFilter === 'mine') return (card.assignees || []).includes(state.me);
  if (state.boardFilter === 'open') return !card.done_at;
  return true;
}

function meta(card) {
  const bits = [];
  if (card.due_date) bits.push(el('span', { class: 'due' + (late(card.due_date) ? ' late' : ''), text: card.due_date }));
  if (card.question) bits.push(el('span', { class: 'q', text: card.question }));
  const list = card.checklist || [];
  if (list.length) bits.push(el('span', { class: 'chk', text: list.filter((i) => i.done).length + '/' + list.length }));
  const notes = (card.comments || []).length;
  if (notes) bits.push(el('span', { class: 'cmt', text: String(notes) }));
  return bits;
}

function cardNode(card) {
  const mine = (card.assignees || []).includes(state.me);
  const who = el('div', { class: 'cw' });
  for (const id of card.assignees || []) who.append(initials(user(id)));
  if (canEdit()) {
    who.append(el('button', {
      class: 'asg', type: 'button', title: 'Assign someone', text: '+',
      onclick: (e) => { e.stopPropagation(); assign(card, e.currentTarget); },
    }));
  }

  const tick = el('button', {
    class: 'tick', type: 'button', text: card.done_at ? 'Done' : 'Mark done',
    onclick: (e) => { e.stopPropagation(); toggleDone(card); },
  });
  const node = el('article', {
    class: 'card' + (card.done_at ? ' done' : '') + (mine ? ' mine' : ''),
    draggable: canEdit() ? 'true' : null, 'data-id': card.id, role: 'button',
  }, who, el('div', { class: 'cb' },
    el('div', { class: 'ct', text: card.title }),
    el('div', { class: 'cm' }, meta(card), canEdit() && tick)));

  node.addEventListener('click', () => openCard(card.id));
  activate(node, () => openCard(card.id));
  node.addEventListener('dragstart', (e) => {
    node.classList.add('dragging');
    // A change arriving mid drag would rebuild the board and take the card out
    // of the hand holding it, so rendering waits until the drop.
    hold(true);
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', String(card.id));
  });
  node.addEventListener('dragend', () => {
    node.classList.remove('dragging');
    for (const c of $$('.col.over')) c.classList.remove('over');
    hold(false);
  });
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

function columnNode(column) {
  const all = columnCards(column.id);
  const shown = all.filter(visible);
  const done = all.filter((c) => c.done_at).length;

  const name = el('h3', { text: column.name, spellcheck: 'false' });
  if (canEdit()) {
    const rename = (e) => {
      e.stopPropagation();
      if (name.isContentEditable) return;
      hold(true);
      editable(name, column.name, (value) => {
        hold(false);
        if (!value || value === column.name) { emit(); return; }
        send('column.rename', { column: column.id, title: value })
          .catch((err) => { say(err.message); emit(); });
      });
    };
    name.addEventListener('click', rename);
    activate(name, rename);
  }

  const cards = el('div', { class: 'cards' });
  for (const card of shown) cards.append(cardNode(card));

  const section = el('section', { class: 'col', 'data-col': column.id },
    el('header', {}, name, el('span', { class: 'mono cnt', text: all.length ? `${done}/${all.length}` : '—' })),
    cards,
    canEdit() && el('button', {
      class: 'add mono', type: 'button', text: '+ Card',
      onclick: () => inlineAdd(column, cards),
    }));

  if (canEdit()) dropZone(section, column, cards);
  return section;
}

function dropZone(section, column, cards) {
  section.addEventListener('dragover', (e) => {
    const dragging = $('#board .card.dragging');
    if (!dragging) return;
    e.preventDefault();
    section.classList.add('over');
    const after = [...cards.querySelectorAll('.card:not(.dragging)')]
      .find((c) => e.clientY < c.getBoundingClientRect().top + c.offsetHeight / 2);
    after ? cards.insertBefore(dragging, after) : cards.append(dragging);
  });
  section.addEventListener('dragleave', () => section.classList.remove('over'));
  section.addEventListener('drop', (e) => {
    e.preventDefault();
    section.classList.remove('over');
    const dragging = $('#board .card.dragging');
    if (!dragging) return;
    const previous = dragging.previousElementSibling;
    send('card.move', {
      card: Number(dragging.dataset.id),
      column: column.id,
      after: previous ? Number(previous.dataset.id) : 0,
    }).catch((err) => { say(err.message); emit(); });
  });
}

// inlineAdd is the one place a card is written straight onto the board. An
// account name in the line puts that person on the card and comes out of it.
function inlineAdd(column, cards) {
  const field = el('textarea', { rows: '1', placeholder: 'What needs doing? @ assigns someone.' });
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
      column: column.id,
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
  const board = clear(into);
  for (const column of state.columns) board.append(columnNode(column));
  if (canEdit()) {
    board.append(el('button', {
      class: 'addcol mono', id: 'addcol', type: 'button', text: '+ Column',
      onclick: () => send('column.create', { proposition: state.open, title: 'New column' })
        .catch((err) => say(err.message)),
    }));
  }
}

export function boardSummary() {
  const cards = [...state.cards.values()];
  return `${cards.filter((c) => c.done_at).length} of ${cards.length} done`;
}
