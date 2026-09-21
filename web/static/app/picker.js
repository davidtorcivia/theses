// The people picker, on a card and in the drawer, and the @ suggestions that
// use the same list inside any field somebody types a name into.

import { $, $$, el, initials } from './dom.js';
import { state } from './state.js';

let pickerAnchor = null;
let mentionField = null;
const outside = () => closePicker();

export function closePicker(restore = false) {
  document.removeEventListener('click', outside);
  const open = $('#picker');
  if (open) open.remove();
  if (pickerAnchor) pickerAnchor.setAttribute('aria-expanded', 'false');
  if (restore && pickerAnchor) pickerAnchor.focus();
  if (mentionField) {
    mentionField.removeAttribute('aria-activedescendant');
    mentionField.removeAttribute('aria-autocomplete');
    mentionField.removeAttribute('aria-controls');
    mentionField.removeAttribute('aria-expanded');
    mentionField.removeAttribute('aria-haspopup');
  }
  pickerAnchor = null;
  mentionField = null;
}

function place(picker, rect) {
  picker.style.left = Math.max(8, Math.min(rect.left, innerWidth - 272)) + 'px';
  picker.style.top = Math.min(rect.bottom + 6, innerHeight - picker.offsetHeight - 12) + 'px';
}

function row(person, on, onPick) {
  return el('button', { type: 'button', role: 'option', 'aria-selected': String(on),
    class: on ? 'on' : '', onclick: (e) => { e.stopPropagation(); onPick(person); } },
    initials(person),
    el('span', { class: 'n', text: person.name }),
    el('span', { class: 'mono role', text: here(person.id) ? 'here' : person.role }),
    el('span', { class: 'st' }));
}

function keyboard(picker, restore) {
  picker.addEventListener('keydown', (e) => {
    const choices = [...picker.querySelectorAll('button')];
    const at = choices.indexOf(document.activeElement);
    let next = -1;
    if (e.key === 'ArrowDown') next = Math.min(choices.length - 1, at + 1);
    if (e.key === 'ArrowUp') next = Math.max(0, at < 0 ? 0 : at - 1);
    if (e.key === 'Home') next = 0;
    if (e.key === 'End') next = choices.length - 1;
    if (next >= 0) { e.preventDefault(); choices[next]?.focus(); }
    if (e.key === 'Escape') { e.preventDefault(); closePicker(restore); }
  });
}

function here(id) {
  return state.presence.some((p) => p.id === id);
}

// openPicker is assign and unassign: the list stays open so several people can
// go on one card in a row.
export function openPicker(anchor, assigned, toggle) {
  closePicker();
  pickerAnchor = anchor;
  anchor.setAttribute('aria-haspopup', 'listbox');
  anchor.setAttribute('aria-expanded', 'true');
  const on = new Set(assigned);
  const proposition = state.props.find((item) => item.id === state.open);
  const picker = el('div', { id: 'picker', role: 'listbox', 'aria-multiselectable': 'true' },
    el('div', { class: 'pt mono', text: 'Assign · choose to toggle' }));
  for (const person of state.users.values()) {
    if (!canAssign(person, on, proposition)) continue;
    picker.append(row(person, on.has(person.id), (p) => {
      const now = !on.has(p.id);
      if (now) on.add(p.id); else on.delete(p.id);
      toggle(p, now);
      for (const b of $$('button', picker)) {
        if (Number(b.dataset.u) === p.id) {
          b.classList.toggle('on', now);
          b.setAttribute('aria-selected', String(now));
        }
      }
    }));
    picker.lastChild.dataset.u = person.id;
  }
  picker.addEventListener('click', (e) => e.stopPropagation());
  document.body.append(picker);
  place(picker, anchor.getBoundingClientRect());
  keyboard(picker, true);
  picker.querySelector('button')?.focus();
  setTimeout(() => document.addEventListener('click', outside, { once: true }), 0);
}

export function canAssign(person, assigned, proposition) {
  return person.role === 'owner' || assigned.has(person.id) || (proposition?.members || []).includes(person.id);
}

// mentionable offers the account names as somebody types @ into a field, in a
// textarea or in a contenteditable.
export function mentionable(field) {
  const read = () => (field.value !== undefined ? field.value : field.textContent);
  const caret = () => {
    if (field.value !== undefined) return field.selectionStart;
    const sel = getSelection();
    return sel.rangeCount ? sel.getRangeAt(0).startOffset : read().length;
  };

  field.addEventListener('input', () => {
    const typed = read().slice(0, caret()).match(/(?:^|\s)@([a-z0-9-]*)$/i);
    closePicker();
    if (!typed) return;
    const q = typed[1].toLowerCase();
    const hits = [...state.users.values()].filter((u) =>
      u.handle.startsWith(q) || u.name.toLowerCase().startsWith(q) || u.initials.toLowerCase().startsWith(q));
    if (!hits.length) return;

    const picker = el('div', { id: 'picker', role: 'listbox' }, el('div', { class: 'pt mono', text: 'Mention' }));
    for (const [index, person] of hits.entries()) {
      const choose = () => {
        const at = caret();
        const before = read().slice(0, at).replace(/@[a-z0-9-]*$/i, '@' + person.handle + ' ');
        const rest = before + read().slice(at);
        if (field.value !== undefined) {
          field.value = rest;
          field.focus();
          field.setSelectionRange(before.length, before.length);
        } else {
          writeContenteditable(field, rest, before.length);
        }
        closePicker();
      };
      const button = row(person, false, choose);
      button.id = 'mention-option-' + index;
      button.tabIndex = -1;
      button.addEventListener('mousedown', (e) => {
        e.preventDefault();
      });
      picker.append(button);
    }
    mentionField = field;
    field.setAttribute('aria-haspopup', 'listbox');
    field.setAttribute('aria-controls', 'picker');
    field.setAttribute('aria-expanded', 'true');
    field.setAttribute('aria-autocomplete', 'list');
    document.body.append(picker);
    place(picker, field.getBoundingClientRect());
    selectMention(picker, field, 0);
    setTimeout(() => document.addEventListener('click', outside, { once: true }), 0);
  });

  field.addEventListener('keydown', (e) => {
    const picker = $('#picker');
    if (!picker || mentionField !== field) return;
    handleMentionKey(e, picker, field);
  });
}

export function writeContenteditable(field, text, at) {
  field.textContent = text;
  field.focus();
  const selection = getSelection();
  if (!selection || !field.firstChild) return;
  const range = document.createRange();
  range.setStart(field.firstChild, at);
  range.collapse(true);
  selection.removeAllRanges();
  selection.addRange(range);
}

export function handleMentionKey(e, picker, field) {
  const choices = [...picker.querySelectorAll('button')];
  const at = choices.findIndex((choice) => choice.getAttribute('aria-selected') === 'true');
  if (e.key === 'Enter') {
    e.preventDefault();
    e.stopImmediatePropagation();
    choices[Math.max(0, at)]?.click();
    return;
  }
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp' || e.key === 'Home' || e.key === 'End') {
    e.preventDefault();
    e.stopImmediatePropagation();
    let next = at;
    if (e.key === 'ArrowDown') next = Math.min(choices.length - 1, at + 1);
    if (e.key === 'ArrowUp') next = Math.max(0, at - 1);
    if (e.key === 'Home') next = 0;
    if (e.key === 'End') next = choices.length - 1;
    selectMention(picker, field, next);
    return;
  }
  if (e.key === 'Tab') { closePicker(); return; }
  if (e.key === 'Escape') {
    e.preventDefault();
    e.stopImmediatePropagation();
    closePicker();
  }
}

function selectMention(picker, field, index) {
  const choices = [...picker.querySelectorAll('button')];
  for (const [at, choice] of choices.entries()) {
    const selected = at === index;
    choice.classList.toggle('active', selected);
    choice.setAttribute('aria-selected', String(selected));
  }
  const active = choices[index];
  if (active) field.setAttribute('aria-activedescendant', active.id);
}
