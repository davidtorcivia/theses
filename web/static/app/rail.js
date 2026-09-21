// The rail: every proposition grouped by where it is, draggable into order,
// with the menu each one carries and the form at the foot that starts a new one.

import { $, $$, el, children, num, ask, say, editable } from './dom.js';
import { state, emit, hold, proposition } from './state.js';
import { send } from './net.js';
import { activate } from './keys.js';
import { movable, carrying } from './drag.js';

function groups() {
  const statuses = state.statuses;
  const first = statuses[0] || 'idea';
  const last = statuses[statuses.length - 1] || 'released';
  const live = [], ideas = [], out = [];
  for (const p of state.props) {
    if (p.kind === 'show' || p.archived_at) continue;
    if (p.status === first) ideas.push(p);
    else if (p.status === last) out.push(p);
    else live.push(p);
  }
  return [
    { id: 'g-active', name: 'In production', items: live },
    { id: 'g-idea', name: 'Ideas', items: ideas },
    { id: 'g-released', name: 'Released', items: out },
  ].filter((g) => g.items.length);
}

// drawn is the node each proposition was last rendered as, with the key it was
// built from, for the same reason the board keeps its cards: a row is carried
// by the pointer, and every applied event on this workspace rebuilt the rail.
const drawn = new Map();

function entry(p) {
  const released = state.statuses[state.statuses.length - 1] || 'released';
  const key = JSON.stringify([p.kind, p.number, p.title, p.status, p.episode, p.archived_at,
    p.id === state.open, state.can.edit, state.can.delete, released]);
  const was = drawn.get(p.id);
  if (was && was.key === key) return was.node;

  // A kept node holds the closures it was built with, and the row behind it is
  // replaced by every event that touches the proposition. So the listeners ask
  // the state for the row when they run rather than reading the one they closed
  // over: the statement and the blurb a rename sends back are not in the key
  // above, and a node built before somebody edited the statement in the work
  // area would put the old one back.
  const id = p.id;
  let tail = p.kind === 'show' ? '' : p.status;
  if (p.archived_at) tail = 'archived';
  else if (p.status === released && p.episode) tail = 'ep ' + p.episode;
  const title = el('span', { class: 't', text: p.title + (tail ? ' ' : '') },
    tail ? el('span', { class: 'st', text: tail }) : null);
  const menu = el('div', { class: 'menu', hidden: true });
  const li = el('li', {
    class: 'ws' + (p.kind === 'show' ? ' showpin' : '') +
      (p.archived_at ? ' arch' : '') + (p.id === state.open ? ' on' : ''),
    'data-n': p.id, 'data-status': p.status,
  }, p.kind === 'show' ? null : el('span', { class: 'no', text: num(p.number) }), title);

  if (state.can.edit) {
    const more = el('button', { class: 'more', 'aria-label': 'More', type: 'button', text: '…' });
    more.addEventListener('click', (e) => {
      e.stopPropagation();
      const wasOpen = !menu.hidden;
      for (const m of $$('#rail .menu')) m.hidden = true;
      menu.hidden = wasOpen;
    });
    const menu_items = p.kind === 'show'
      ? [['Rename', 'rename'], ['Settings', 'settings']]
      : p.archived_at
      ? [['Restore', 'archive'], ['Delete', 'delete']]
      : [['Rename', 'rename'], ['Settings', 'settings'], ['Archive', 'archive'], ['Delete', 'delete']];
    for (const [label, act] of menu_items) {
      if (act === 'delete' && !state.can.delete) continue;
      menu.append(el('button', {
        type: 'button', text: label,
        onclick: (e) => { e.stopPropagation(); menu.hidden = true; act === 'rename' ? rename(id, title) : run(act, id); },
      }));
    }
    li.append(more, menu);
  }

  li.addEventListener('click', (e) => {
    if (carrying() || e.target.closest('.menu') || e.target.closest('.more') || title.isContentEditable) return;
    go(id);
  });
  activate(li, () => { if (!title.isContentEditable) go(id); });
  // The same pointer drag the board uses, for the same reason: a finger fires
  // no drag event, so the rail could not be ordered on a phone either. An
  // archived proposition has no place in the order and so is not carried.
  if (p.kind !== 'show' && !p.archived_at) {
    movable(li, {
      zone: '#rail ul.order',
      drop: () => {
        const previous = li.previousElementSibling;
        send('proposition.move', {
          proposition: id,
          after: previous ? Number(previous.dataset.n) : 0,
        }).catch((err) => { say(err.message); emit(); });
      },
    });
  }
  drawn.set(id, { key, node: li });
  return li;
}

function rename(id, title) {
  const p = proposition(id);
  if (!p) return;
  hold(true);
  editable(title, p.title, (value) => {
    hold(false);
    // editable wrote into the node to open it: it put the title's own text
    // there, which took the status tail out of the span beside it, and Escape
    // leaves whatever was typed sitting in it. The render below used to draw
    // the row again out of the state, because the rail was built from nothing
    // every time; now a row whose key has not moved is handed back exactly as
    // the editor left it. So the row is dropped from the cache whichever way
    // the edit ended, a refusal included: one refused in the same frame as its
    // own guess is a single render with the key where it started. Building the
    // row again cannot take one out from under a finger, because rendering was
    // held for as long as the editor was open and the hand that was typing is
    // this one.
    drawn.delete(id);
    const now = proposition(id);
    if (value === null || !value || !now || value === now.title) { emit(); return; }
    send('proposition.edit', { proposition: id, title: value, statement: now.statement, blurb: now.blurb })
      .catch((err) => { say(err.message); emit(); });
  });
}

function run(act, id) {
  const p = proposition(id);
  if (!p) return;
  if (act === 'settings') { location.href = p.kind === 'show' ? '/show/settings' : `/p/${id}/settings`; return; }
  if (act === 'archive') {
    send(p.archived_at ? 'proposition.restore' : 'proposition.archive', { proposition: id })
      .catch((err) => say(err.message));
    return;
  }
  ask(`Delete ${num(p.number)} ${p.title}?`,
    'The board, its cards and everything filed under this proposition go with it. The record of the deletion stays in activity.',
    'Delete permanently').then((yes) => {
    if (!yes) return;
    send('proposition.delete', { proposition: id })
      .then(() => { if (state.open === id) location.href = '/'; })
      .catch((err) => say(err.message));
  });
}

export function go(id) {
  // Closing the active row does not navigate, so chrome must synchronize
  // the narrow rail's focus, inert state and disclosure state.
  const toggle = $('#railtoggle');
  if (document.body.classList.contains('rail-open') && toggle) toggle.click();
  else document.body.classList.remove('rail-open');
  const p = proposition(id);
  if (p && id !== state.open) location.href = p.kind === 'show' ? '/show' : '/p/' + id;
}

// The groups and the lists inside them are made once and kept, so that a row
// nothing happened to is never taken out of the page. Only the heading, which
// carries a count, is drawn again.
const parts = new Map();

function showNode(row, keep) {
  let part = parts.get('show');
  if (!part) {
    part = { node: el('ul', { class: 'show', 'aria-label': 'Show workspace' }) };
    parts.set('show', part);
  }
  children(part.node, [row], keep);
  return part.node;
}

function groupNode(id, name, rows, keep) {
  let part = parts.get(id);
  if (!part) {
    // The lists that hold an order. The archived one below is not one of them,
    // so nothing can be carried into it.
    const list = el('ul', { class: 'order' });
    part = { list, heading: el('h4'), node: null };
    part.node = el('div', { class: 'group', id }, part.heading, list);
    parts.set(id, part);
  }
  part.heading.textContent = name + ' ';
  part.heading.append(el('i', { text: String(rows.length) }));
  children(part.list, rows, keep);
  return part.node;
}

function archivedNode(rows, keep) {
  let part = parts.get('archived');
  if (!part) {
    const list = el('ul', { hidden: true });
    const toggle = el('button', {
      id: 'archtoggle', class: 'h4', type: 'button', text: 'Archived ',
      onclick: () => { list.hidden = !list.hidden; toggle.classList.toggle('open', !list.hidden); },
    }, el('i'));
    part = { list, toggle, node: el('div', { class: 'group', id: 'archived' }, toggle, list) };
    parts.set('archived', part);
  }
  part.toggle.lastChild.textContent = String(rows.length);
  children(part.list, rows, keep);
  return part.node;
}

export function renderRail() {
  // A row that was rebuilt takes the keyboard with it. Its number is taken now
  // and the focus put back at the end, the same way the work area keeps the
  // card somebody had reached. A row that was kept keeps its own focus and this
  // puts it where it already is.
  const focused = document.activeElement;
  const had = focused && focused.classList && focused.classList.contains('ws')
    ? focused.dataset.n : '';
  const rail = $('#rail');

  const filter = el('div', { id: 'tagfilter' });
  for (const status of state.statuses) {
    filter.append(el('button', {
      type: 'button', text: status, 'data-t': status,
      class: state.railFilter === status ? 'on' : '',
      onclick: () => { state.railFilter = state.railFilter === status ? null : status; emit(); },
    }));
  }

  // Every row is built before any list is swept, for the reason the board has:
  // a proposition whose status moved it to another group is still wanted, and
  // taking it out of the group it was in first would drop it out of the page.
  const shown = groups()
    .map((group) => ({
      ...group,
      rows: group.items
        .filter((p) => !state.railFilter || p.status === state.railFilter)
        .map(entry),
    }))
    .filter((group) => group.rows.length);
  // The same entry as any other, so an archived proposition still opens and
  // still carries the two things its menu has left, restore and delete.
  const show = state.props.find((p) => p.kind === 'show');
  const showRow = show ? entry(show) : null;
  const archived = state.props.filter((p) => p.kind !== 'show' && p.archived_at).map(entry);
  const keep = new Set([showRow, ...shown.flatMap((group) => group.rows), ...archived].filter(Boolean));

  children(rail, [
    showRow && showNode(showRow, keep),
    el('div', { class: 'railtop' }, filter),
    shown.map((group) => groupNode(group.id, group.name, group.rows, keep)),
    archived.length && archivedNode(archived, keep),
    state.can.edit && foot(),
  ]);

  const live = new Set(state.props.map((p) => p.id));
  for (const id of drawn.keys()) if (!live.has(id)) drawn.delete(id);

  if (had) {
    const row = $(`.ws[data-n="${had}"]`, rail);
    if (row) row.focus();
  }
}

// The form at the foot is made once: it holds what somebody has typed into it
// and whether it is open, and a render must not take either away.
let footer = null;

function foot() {
  if (footer) return footer;
  const input = el('input', { placeholder: 'Title, then Enter', spellcheck: 'false' });
  const form = el('div', { id: 'newform', hidden: true }, input);
  const button = el('button', {
    id: 'newprop', type: 'button', text: '+ New proposition',
    onclick: () => { form.hidden = !form.hidden; if (!form.hidden) input.focus(); },
  });
  input.addEventListener('focus', () => hold(true));
  input.addEventListener('blur', () => hold(false));
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { form.hidden = true; return; }
    if (e.key !== 'Enter' || !input.value.trim()) return;
    const title = input.value.trim();
    input.value = '';
    form.hidden = true;
    hold(false);
    send('proposition.create', { title })
      .then((ev) => {
        // A command that was kept rather than sent has no event yet, and so no
        // proposition to open.
        if (!ev) { say('Kept on this device. It is made when the connection is back.'); return; }
        location.href = `/p/${ev.entity_id}/settings`;
      })
      .catch((err) => say(err.message));
  });
  return footer = el('div', { class: 'railfoot' }, button, form);
}

// A click anywhere else closes whichever rail menu is open.
document.addEventListener('click', (e) => {
  if (e.target.closest('#rail .ws')) return;
  for (const m of $$('#rail .menu')) m.hidden = true;
});
