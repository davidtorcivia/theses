// The rail: every proposition grouped by where it is, draggable into order,
// with the menu each one carries and the form at the foot that starts a new one.

import { $, $$, el, clear, num, ask, say, editable } from './dom.js';
import { state, emit, hold } from './state.js';
import { send } from './net.js';
import { activate } from './keys.js';

function groups() {
  const statuses = state.statuses;
  const first = statuses[0] || 'idea';
  const last = statuses[statuses.length - 1] || 'released';
  const live = [], ideas = [], out = [];
  for (const p of state.props) {
    if (p.archived_at) continue;
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

function entry(p) {
  const released = state.statuses[state.statuses.length - 1] || 'released';
  let tail = p.status;
  if (p.archived_at) tail = 'archived';
  else if (p.status === released && p.episode) tail = 'ep ' + p.episode;
  const title = el('span', { class: 't', text: p.title + ' ' },
    el('span', { class: 'st', text: tail }));
  const menu = el('div', { class: 'menu', hidden: true });
  const li = el('li', {
    class: 'ws' + (p.archived_at ? ' arch' : '') + (p.id === state.open ? ' on' : ''),
    'data-n': p.id, 'data-status': p.status, draggable: p.archived_at ? null : 'true',
  }, el('span', { class: 'no', text: num(p.number) }), title);

  if (state.can.edit) {
    const more = el('button', { class: 'more', 'aria-label': 'More', type: 'button', text: '…' });
    more.addEventListener('click', (e) => {
      e.stopPropagation();
      const wasOpen = !menu.hidden;
      for (const m of $$('#rail .menu')) m.hidden = true;
      menu.hidden = wasOpen;
    });
    const menu_items = p.archived_at
      ? [['Restore', 'archive'], ['Delete', 'delete']]
      : [['Rename', 'rename'], ['Settings', 'settings'], ['Archive', 'archive'], ['Delete', 'delete']];
    for (const [label, act] of menu_items) {
      if (act === 'delete' && !state.can.delete) continue;
      menu.append(el('button', {
        type: 'button', text: label,
        onclick: (e) => { e.stopPropagation(); menu.hidden = true; act === 'rename' ? rename(p, title) : run(act, p); },
      }));
    }
    li.append(more, menu);
  }

  li.addEventListener('click', (e) => {
    if (e.target.closest('.menu') || e.target.closest('.more') || title.isContentEditable) return;
    go(p.id);
  });
  activate(li, () => { if (!title.isContentEditable) go(p.id); });
  if (!p.archived_at) {
    li.addEventListener('dragstart', (e) => {
      li.classList.add('dragging');
      hold(true);
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', String(p.id));
    });
    li.addEventListener('dragend', () => { li.classList.remove('dragging'); hold(false); });
  }
  return li;
}

function rename(p, title) {
  hold(true);
  editable(title, p.title, (value) => {
    hold(false);
    if (value === null || value === p.title || !value) { emit(); return; }
    send('proposition.edit', { proposition: p.id, title: value, statement: p.statement, blurb: p.blurb })
      .catch((err) => { say(err.message); emit(); });
  });
}

function run(act, p) {
  if (act === 'settings') { location.href = `/p/${p.id}/settings`; return; }
  if (act === 'archive') {
    send(p.archived_at ? 'proposition.restore' : 'proposition.archive', { proposition: p.id })
      .catch((err) => say(err.message));
    return;
  }
  ask(`Delete ${num(p.number)} ${p.title}?`,
    'The board, its cards and everything filed under this proposition go with it. The record of the deletion stays in activity.',
    'Delete permanently').then((yes) => {
    if (!yes) return;
    send('proposition.delete', { proposition: p.id })
      .then(() => { if (state.open === p.id) location.href = '/'; })
      .catch((err) => say(err.message));
  });
}

export function go(id) {
  document.body.classList.remove('rail-open');
  if (id !== state.open) location.href = '/p/' + id;
}

function droppable(list) {
  list.addEventListener('dragover', (e) => {
    const dragging = $('#rail .ws.dragging');
    if (!dragging) return;
    e.preventDefault();
    const after = [...list.querySelectorAll('.ws:not(.dragging)')]
      .find((x) => e.clientY < x.getBoundingClientRect().top + x.offsetHeight / 2);
    after ? list.insertBefore(dragging, after) : list.append(dragging);
  });
  list.addEventListener('drop', (e) => {
    e.preventDefault();
    const dragging = $('#rail .ws.dragging');
    if (!dragging) return;
    const previous = dragging.previousElementSibling;
    send('proposition.move', {
      proposition: Number(dragging.dataset.n),
      after: previous ? Number(previous.dataset.n) : 0,
    }).catch((err) => { say(err.message); emit(); });
  });
}

export function renderRail() {
  // The rail is built again from nothing on every render, so a row holding the
  // keyboard goes with it. Its number is taken now and the focus put back at
  // the end, the same way the work area keeps the card somebody had reached.
  const focused = document.activeElement;
  const had = focused && focused.classList && focused.classList.contains('ws')
    ? focused.dataset.n : '';
  const rail = clear($('#rail'));

  const filter = el('div', { id: 'tagfilter' });
  for (const status of state.statuses) {
    filter.append(el('button', {
      type: 'button', text: status, 'data-t': status,
      class: state.railFilter === status ? 'on' : '',
      onclick: () => { state.railFilter = state.railFilter === status ? null : status; emit(); },
    }));
  }
  rail.append(el('div', { class: 'railtop' }, filter));

  for (const group of groups()) {
    const items = group.items.filter((p) => !state.railFilter || p.status === state.railFilter);
    if (!items.length) continue;
    const list = el('ul', {});
    for (const p of items) list.append(entry(p));
    droppable(list);
    rail.append(el('div', { class: 'group', id: group.id },
      el('h4', { text: group.name + ' ' }, el('i', { text: String(items.length) })),
      list));
  }

  const archived = state.props.filter((p) => p.archived_at);
  if (archived.length) {
    const list = el('ul', { hidden: true });
    // The same entry as any other, so an archived proposition still opens and
    // still carries the two things its menu has left, restore and delete.
    for (const p of archived) list.append(entry(p));
    const toggle = el('button', {
      id: 'archtoggle', class: 'h4', type: 'button', text: 'Archived ',
      onclick: () => { list.hidden = !list.hidden; toggle.classList.toggle('open', !list.hidden); },
    }, el('i', { text: String(archived.length) }));
    rail.append(el('div', { class: 'group', id: 'archived' }, toggle, list));
  }

  if (state.can.edit) rail.append(foot());
  if (had) {
    const row = $(`.ws[data-n="${had}"]`, rail);
    if (row) row.focus();
  }
}

function foot() {
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
  return el('div', { class: 'railfoot' }, button, form);
}

// A click anywhere else closes whichever rail menu is open.
document.addEventListener('click', (e) => {
  if (e.target.closest('#rail .ws')) return;
  for (const m of $$('#rail .menu')) m.hidden = true;
});
