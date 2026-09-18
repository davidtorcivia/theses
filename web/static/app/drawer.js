// The card drawer: assignees, due, question, column, state, description,
// checklist and the activity with its notes. Everything in it is a command, and
// the two text fields carry the version they started from.

import { $, el, clear, add, initials, inline, say, editable } from './dom.js';
import { state, user, byHandle, emit, hold } from './state.js';
import { send, where, Conflict } from './net.js';
import { openPicker, closePicker, mentionable } from './picker.js';

export function closeDrawer() {
  state.openCard = null;
  $('#drawer').hidden = true;
  document.body.classList.remove('has-drawer');
  where('');
  emit();
}

export function openCard(id) {
  if (state.openCard !== id) {
    state.openCard = id;
    where('card:' + id);
  }
  emit();
}

const rendered = (text) => inline(text, byHandle);

export function renderDrawer() {
  const drawer = $('#drawer');
  const card = state.cards.get(state.openCard);
  if (!card) {
    drawer.hidden = true;
    document.body.classList.remove('has-drawer');
    return;
  }
  const top = drawer.scrollTop;
  clear(drawer);
  drawer.hidden = false;
  document.body.classList.add('has-drawer');

  const column = state.columns.find((c) => c.id === card.column_id);
  drawer.append(el('div', { class: 'dh' },
    el('span', { class: 'mono', text: (column ? column.name : '') + ' · ' + (card.done_at ? 'done' : 'open') }),
    el('button', { class: 'x', type: 'button', text: 'Close', onclick: closeDrawer })));

  const heading = el('h2', { text: card.title, spellcheck: 'false' });
  if (state.can.edit) {
    heading.addEventListener('click', () => {
      if (heading.isContentEditable) return;
      hold(true);
      editable(heading, card.title, (value) => {
        hold(false);
        if (!value || value === card.title) { emit(); return; }
        versioned('card.title', card, { title: value }, heading, card.title);
      });
    });
  }
  drawer.append(heading);
  drawer.append(props(card));

  drawer.append(el('h4', { text: 'Description' }));
  drawer.append(description(card));

  drawer.append(el('h4', { text: 'Checklist' }));
  drawer.append(checklist(card));

  drawer.append(el('h4', { text: 'Linked material' }));
  drawer.append(el('ul', { class: 'linked' },
    el('li', { class: 'dim', text: 'Nothing linked yet. Links and files arrive in the next step.' })));

  drawer.append(el('h4', { text: 'Activity' }));
  drawer.append(activity(card));
  if (state.can.edit) drawer.append(noteForm(card));
  drawer.scrollTop = top;
}

function props(card) {
  const who = el('dd', { class: 'who' });
  for (const id of card.assignees || []) {
    const person = user(id);
    who.append(el('button', {
      class: 'rm', type: 'button', title: 'Unassign ' + person.name,
      onclick: () => send('card.unassign', { card: card.id, user: id }).catch((e) => say(e.message)),
    }, initials(person)));
  }
  if (state.can.edit) {
    who.append(el('button', {
      class: 'lnk', type: 'button', text: '+ assign',
      onclick: (e) => {
        e.stopPropagation();
        openPicker(e.currentTarget, card.assignees || [], (person, on) =>
          send(on ? 'card.assign' : 'card.unassign', { card: card.id, user: person.id })
            .catch((err) => say(err.message)));
      },
    }));
  }

  const due = el('button', { class: 'lnk plain', type: 'button' },
    card.due_date ? document.createTextNode(card.due_date) : el('span', { class: 'dim', text: 'set a date' }));
  due.addEventListener('click', () => {
    if (!state.can.edit) return;
    const field = el('input', { class: 'inline', value: card.due_date || '', placeholder: 'e.g. 24 Sep' });
    due.replaceWith(field);
    hold(true);
    field.focus();
    let done = false;
    const finish = (save) => {
      if (done) return;
      done = true;
      hold(false);
      if (!save) { emit(); return; }
      send('card.due', { card: card.id, due: field.value.trim() }).catch((e) => say(e.message));
    };
    field.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') finish(true);
      if (e.key === 'Escape') finish(false);
    });
    field.addEventListener('blur', () => finish(true));
  });

  const question = el('select', {
    disabled: !state.can.edit,
    onchange: (e) => send('card.question', { card: card.id, question: e.target.value }).catch((x) => say(x.message)),
  }, el('option', { value: '', text: 'none', selected: !card.question }));
  state.questions.forEach((q, i) => {
    question.append(el('option', {
      value: q, selected: q === card.question,
      text: q + ' · ' + (state.questionLabels[i] || ''),
    }));
  });

  const columns = el('select', {
    disabled: !state.can.edit,
    onchange: (e) => send('card.move', { card: card.id, column: Number(e.target.value), after: 0 })
      .catch((x) => say(x.message)),
  });
  for (const c of state.columns) {
    columns.append(el('option', { value: c.id, text: c.name, selected: c.id === card.column_id }));
  }

  return el('dl', { class: 'props' },
    el('dt', { text: 'Assigned' }), who,
    el('dt', { text: 'Due' }), el('dd', {}, due),
    el('dt', { text: 'Question' }), el('dd', {}, question),
    el('dt', { text: 'Column' }), el('dd', {}, columns),
    el('dt', { text: 'State' }), el('dd', {}, el('button', {
      class: 'lnk plain', type: 'button', text: card.done_at ? 'Done · reopen' : 'Open · mark done',
      onclick: () => send('card.done', { card: card.id, done: !card.done_at }).catch((e) => say(e.message)),
    })));
}

function description(card) {
  const node = el('p', {
    class: 'desc', 'data-ph': 'Add a description. @ mentions notify people.',
    contenteditable: state.can.edit ? 'true' : null, spellcheck: 'false',
  });
  add(node, [rendered(card.description_md || '')]);
  if (!state.can.edit) return node;

  mentionable(node);
  node.addEventListener('focus', () => {
    hold(true);
    node.textContent = card.description_md || '';
  });
  node.addEventListener('blur', () => {
    hold(false);
    const value = node.textContent.trim();
    if (value === (card.description_md || '')) { emit(); return; }
    versioned('card.description', card, { text: value }, node, value);
  });
  return node;
}

// versioned sends a text edit with the version it started from and, when the
// answer is a conflict, puts the choice in the page rather than in an alert.
function versioned(cmd, card, args, anchor, mine) {
  send(cmd, { card: card.id, base: card.version, ...args }).catch((err) => {
    if (!(err instanceof Conflict)) { say(err.message); emit(); return; }
    anchor.after(conflictBar(err.detail, () => {
      send(cmd, { card: card.id, base: err.detail.version, ...args }).catch((e) => say(e.message));
    }));
  });
}

function conflictBar(detail, keepMine) {
  const bar = el('p', { class: 'notice bad' },
    'Somebody changed this while you were editing it. Theirs reads ',
    el('span', { class: 'mono', text: detail.current || '(nothing)' }), '. ');
  bar.append(el('button', {
    class: 'lnk', type: 'button', text: 'Keep mine',
    onclick: () => { bar.remove(); keepMine(); },
  }), ' ', el('button', {
    class: 'lnk plain', type: 'button', text: 'Take theirs',
    onclick: () => { bar.remove(); emit(); },
  }));
  return bar;
}

function checklist(card) {
  const list = el('ul', { class: 'check' });
  for (const item of card.checklist || []) {
    list.append(el('li', { class: item.done ? 'd' : '' },
      el('label', {},
        el('input', {
          type: 'checkbox', checked: item.done, disabled: !state.can.edit,
          onchange: (e) => send('checklist.toggle', { item: item.id, done: e.target.checked })
            .catch((x) => say(x.message)),
        }),
        ' ' + item.text),
      state.can.edit && el('button', {
        class: 'lnk del quiet', type: 'button', text: 'remove',
        onclick: () => send('checklist.remove', { item: item.id }).catch((e) => say(e.message)),
      })));
  }
  if (state.can.edit) {
    const field = el('input', { class: 'newitem', placeholder: '+ item' });
    field.addEventListener('focus', () => hold(true));
    field.addEventListener('blur', () => hold(false));
    field.addEventListener('keydown', (e) => {
      if (e.key !== 'Enter' || !field.value.trim()) return;
      const text = field.value.trim();
      field.value = '';
      send('checklist.add', { card: card.id, text }).catch((err) => say(err.message));
    });
    list.append(el('li', {}, field));
  }
  return list;
}

function activity(card) {
  const notes = card.comments || [];
  const list = el('ol', { class: 'comments' });
  if (!notes.length) {
    list.append(el('li', { class: 'dim', text: 'Nothing yet.' }));
    return list;
  }
  for (const note of notes) {
    const person = user(note.user_id);
    const body = el('p', {});
    add(body, [rendered(note.body_md)]);
    const line = el('div', {}, body,
      el('span', { class: 'mono when', text: when(note.created_at) }));
    if (note.user_id === state.me) {
      line.append(' ', el('button', {
        class: 'lnk del quiet', type: 'button', text: 'delete',
        onclick: () => send('comment.delete', { comment: note.id }).catch((e) => say(e.message)),
      }));
    }
    list.append(el('li', {}, initials(person), line));
  }
  return list;
}

function when(unix) {
  const seconds = Math.max(0, Math.floor(Date.now() / 1000) - unix);
  if (seconds < 60) return 'just now';
  if (seconds < 3600) return Math.floor(seconds / 60) + ' min ago';
  if (seconds < 86400) return Math.floor(seconds / 3600) + ' h ago';
  return new Date(unix * 1000).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

function noteForm(card) {
  const field = el('textarea', { rows: '1', placeholder: 'Write a note. @ to mention someone. ⌘↵ posts.' });
  mentionable(field);
  field.addEventListener('focus', () => hold(true));
  field.addEventListener('blur', () => hold(false));
  const post = () => {
    const body = field.value.trim();
    if (!body) return;
    field.value = '';
    closePicker();
    hold(false);
    send('comment.post', { card: card.id, text: body }).catch((err) => say(err.message));
  };
  field.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); post(); }
  });
  return el('div', { class: 'cf' }, field,
    el('button', { class: 'lnk', type: 'button', text: 'Post', onclick: post }));
}
