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
  if (state.boardFilter === 'mine') return (card.assignees || []).includes(state.me);
  if (state.boardFilter === 'open') return !card.done_at;
  return true;
}

function meta(card) {
  const bits = [];
  if (card.due_date) bits.push(el('span', { class: 'due' + (late(card) ? ' late' : ''), text: card.due_date }));
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
    'data-id': card.id,
  }, who, el('div', { class: 'cb' },
    el('div', { class: 'ct', text: card.title }),
    el('div', { class: 'cm' }, meta(card), canEdit() && tick)));

  node.addEventListener('click', () => {
    // The pointer that has just carried a card ends in a click as well, and
    // that one finishes the drag rather than asking to read the card.
    if (carried) return;
    openCard(card.id);
  });
  activate(node, () => openCard(card.id));
  if (canEdit()) drag(node, card);
  return node;
}

// A phone fires no HTML5 drag event from a finger, so on one that mechanism
// cannot reach the board at all. Pointer events are the single path that
// carries a mouse, a pen and a finger, and this is the whole of the drag.
//
// Only the moment a drag begins differs between them. A mouse with the button
// down that has moved a few pixels is dragging, because a mouse never scrolls
// the page this way. A finger is doing one of three things, and they are told
// apart in time: one that stays put long enough for the press to be meant is
// carrying the card, one that moves before then is scrolling the page, and one
// that lifts before then has tapped to open the card.
const PRESS = 300;
const SLOP = 6;

// How near an edge brings the page up to meet the finger, and how fast.
const EDGE = 64;
const SPEED = 12;

let carried = false;

function drag(node, card) {
  node.addEventListener('pointerdown', (e) => {
    carried = false;
    // The tick and the assign button answer for themselves, and a second
    // button is not a drag.
    if (e.button !== 0 || e.target.closest('button')) return;

    const mouse = e.pointerType === 'mouse';
    const from = { x: e.clientX, y: e.clientY };
    let at = { ...from };
    let grab = null;
    let ghost = null;
    let on = false;
    let edge = 0;
    let frame = 0;
    let press = mouse ? 0 : setTimeout(start, PRESS);

    // Neither value of touch-action answers this. The browser reads the rule
    // when the finger lands, a third of a second before the press that picks
    // the card up, so a rule written at either moment is read too late to stop
    // the scroll. Refusing the first touchmove of the drag does stop it: the
    // press needed the finger still, so no scroll has begun to refuse.
    const still = (ev) => ev.preventDefault();
    // Android answers a long press with a context menu and cancels the pointer
    // behind it, which would drop the card in the moment it was picked up.
    const quiet = (ev) => ev.preventDefault();

    node.setPointerCapture(e.pointerId);
    addEventListener('pointermove', moved);
    addEventListener('pointerup', up);
    addEventListener('pointercancel', end);
    addEventListener('contextmenu', quiet);
    // Ahead of the listener on the document, which would read the same key as
    // a request to close whatever else is open.
    addEventListener('keydown', abandon, true);

    function start() {
      press = 0;
      on = true;
      const box = node.getBoundingClientRect();
      grab = { x: at.x - box.left, y: at.y - box.top };
      ghost = node.cloneNode(true);
      ghost.classList.add('ghost');
      ghost.removeAttribute('tabindex');
      ghost.style.width = box.width + 'px';
      document.body.append(ghost);
      node.classList.add('dragging');
      // A change arriving mid drag would rebuild the board and take the card
      // out of the hand holding it, so rendering waits until the drop.
      hold(true);
      addEventListener('touchmove', still, { passive: false });
      frame = requestAnimationFrame(tick);
      follow();
    }

    function follow() {
      ghost.style.left = (at.x - grab.x) + 'px';
      ghost.style.top = (at.y - grab.y) + 'px';
    }

    // place puts the card where the pointer is, by the rule the mockup drew:
    // among the cards of the column under the pointer, above the first one
    // whose middle the pointer has not reached.
    function place() {
      const col = document.elementFromPoint(at.x, at.y)?.closest('.col');
      if (!col) return;
      for (const other of $$('.col.over')) if (other !== col) other.classList.remove('over');
      col.classList.add('over');
      const cards = $('.cards', col);
      const after = [...cards.querySelectorAll('.card:not(.dragging)')]
        .find((c) => at.y < c.getBoundingClientRect().top + c.offsetHeight / 2);
      after ? cards.insertBefore(node, after) : cards.append(node);
    }

    // A finger at the edge of a phone cannot reach the column below the fold,
    // so the page comes to it. The pointer holds still while this runs and the
    // board moves under it, so the card is placed again on every frame.
    function tick() {
      if (edge) { scrollBy(0, edge); place(); }
      frame = requestAnimationFrame(tick);
    }

    function moved(ev) {
      if (ev.pointerId !== e.pointerId) return;
      at = { x: ev.clientX, y: ev.clientY };
      if (!on) {
        if (Math.abs(at.x - from.x) <= SLOP && Math.abs(at.y - from.y) <= SLOP) return;
        if (mouse) start(); else end();
        return;
      }
      follow();
      place();
      edge = mouse ? 0 : at.y < EDGE ? -SPEED : at.y > innerHeight - EDGE ? SPEED : 0;
    }

    function up(ev) {
      if (ev.pointerId !== e.pointerId) return;
      if (on) {
        carried = true;
        const previous = node.previousElementSibling;
        send('card.move', {
          card: card.id,
          column: Number(node.closest('.col').dataset.col),
          after: previous ? Number(previous.dataset.id) : 0,
        }).catch((err) => { say(err.message); emit(); });
      }
      end();
    }

    // Escape puts the card down. Nothing is sent, and the render held through
    // the drag draws the board back out of the state, which is where the card
    // never stopped being.
    function abandon(ev) {
      if (ev.key !== 'Escape' || !on) return;
      ev.preventDefault();
      ev.stopPropagation();
      end();
    }

    function end() {
      clearTimeout(press);
      cancelAnimationFrame(frame);
      removeEventListener('pointermove', moved);
      removeEventListener('pointerup', up);
      removeEventListener('pointercancel', end);
      removeEventListener('contextmenu', quiet);
      removeEventListener('keydown', abandon, true);
      removeEventListener('touchmove', still);
      if (node.hasPointerCapture(e.pointerId)) node.releasePointerCapture(e.pointerId);
      if (!on) return;
      on = false;
      ghost.remove();
      node.classList.remove('dragging');
      for (const col of $$('.col.over')) col.classList.remove('over');
      hold(false);
    }
  });
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

  return section;
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
