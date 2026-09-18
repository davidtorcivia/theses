// The card drawer: assignees, due, question, column, state, description,
// checklist and the activity with its notes. Everything in it is a command, and
// the two text fields carry the version they started from.

import { $, el, clear, add, initials, inline, say, editable, ask } from './dom.js';
import { state, user, byHandle, emit, hold, canEdit, material } from './state.js';
import { send, where, Conflict } from './net.js';
import { openPicker, closePicker, mentionable } from './picker.js';
import { renderLinkDrawer, attachedLinks, host } from './links.js';
import { renderFileDrawer, attachedFiles, bytes } from './files.js';
import { renderPanel, closePanel } from './activity.js';
import { activate } from './keys.js';
import * as api from './api.js';

// takeFocus and returnTo carry the keyboard across a render. Opening a card
// puts the focus in the drawer and closing it puts the focus back on the card
// that was open, neither of which can be done here: the board and the drawer
// are both built again after this runs, and the nodes to focus do not exist
// until they are.
let takeFocus = 0;
let returnTo = 0;

export function closeDrawer() {
  closePanel();
  returnTo = state.openCard;
  state.openCard = state.openLink = state.openFile = null;
  state.conflict = {};
  $('#drawer').hidden = true;
  document.body.classList.remove('has-drawer');
  where('');
  emit();
}

export function openCard(id) {
  if (state.openCard !== id) {
    state.openCard = id;
    state.conflict = {};
    takeFocus = id;
    where('card:' + id);
  }
  state.openLink = state.openFile = null;
  emit();
}

// linked is the links and files hanging off this card, with a picker to add
// one. It is the same join the links and files drawers show from their side.
function linked(card) {
  // The card drawer opens on the board, where neither pane has been shown, so
  // the links and files are read here too. material only ever reads them once.
  material().catch(() => {});
  const list = el('ul', { class: 'linked' });
  for (const link of attachedLinks(card.id)) {
    list.append(el('li', {},
      el('a', { href: link.url, target: '_blank', rel: 'noopener noreferrer',
        text: link.title || host(link.url) }),
      el('span', { class: 'mono dim', text: ' ' + (link.kind || 'link') + ' · ' }),
      canEdit() ? el('button', { class: 'lnk del', type: 'button', text: 'Detach',
        onclick: () => detach('links', card.id, link.id) }) : null));
  }
  for (const file of attachedFiles(card.id)) {
    list.append(el('li', {},
      el('span', { text: file.name }),
      el('span', { class: 'mono dim', text: ' ' + bytes(file.size) + ' · ' }),
      canEdit() ? el('button', { class: 'lnk del', type: 'button', text: 'Detach',
        onclick: () => detach('files', card.id, file.id) }) : null));
  }
  if (!list.childElementCount) {
    list.append(el('li', { class: 'dim', text: 'Nothing linked yet.' }));
  }
  if (canEdit()) {
    list.append(el('li', {}, el('button', {
      class: 'lnk', type: 'button', text: '+ attach a link or a file',
      onclick: (e) => { e.stopPropagation(); attachDialog(card); },
    })));
  }
  return list;
}

async function detach(what, card, id) {
  try {
    await api.del('/cards/' + card + '/' + what + '/' + id);
  } catch (err) {
    say(err.message);
  }
}

// attachDialog is the picker: everything on this proposition that is not on
// this card already. A real dialog, as the rest of the app uses.
function attachDialog(card) {
  const onCard = new Set([
    ...attachedLinks(card.id).map((l) => 'links/' + l.id),
    ...attachedFiles(card.id).map((f) => 'files/' + f.id),
  ]);
  // The dialog is outside the drawer, so it takes the plain list class rather
  // than the drawer's own, which is where the bullets are turned off.
  const list = el('ul', { class: 'list' });
  const offer = [
    ...state.links.map((l) => ({ what: 'links', id: l.id, label: l.title || host(l.url), kind: l.kind || 'link' })),
    ...state.files.filter((f) => f.state === 'ready')
      .map((f) => ({ what: 'files', id: f.id, label: f.name, kind: f.kind || 'file' })),
  ].filter((row) => !onCard.has(row.what + '/' + row.id));

  const dialog = el('dialog', {},
    el('h3', { text: 'Attach to ' + card.title }),
    list,
    el('div', { class: 'acts' },
      el('button', { class: 'lnk plain', type: 'button', text: 'Done', onclick: () => dialog.close() })));

  if (!offer.length) {
    list.append(el('li', { class: 'dim', text: 'Nothing left to attach. Add a link or a file first.' }));
  }
  for (const row of offer) {
    list.append(el('li', {}, el('button', {
      class: 'lnk', type: 'button',
      onclick: async (e) => {
        // The button is taken before the first await: currentTarget is null
        // once the event has finished being dispatched.
        const button = e.currentTarget;
        button.disabled = true;
        try {
          await api.post('/cards/' + card.id + '/' + row.what + '/' + row.id);
          button.closest('li').remove();
        } catch (err) {
          say(err.message);
          button.disabled = false;
        }
      },
    }, el('span', { class: 'k mono', text: row.kind }), ' ' + row.label)));
  }
  dialog.addEventListener('close', () => dialog.remove());
  document.body.append(dialog);
  dialog.showModal();
}

const rendered = (text) => inline(text, byHandle);

// restoreFocus puts the keyboard back on the card the drawer was showing, now
// that the board has been drawn again and that card is a node once more. It
// keeps doing so across the renders that follow, because each one builds the
// board again and drops the focus, and it stops the moment the keyboard is
// somewhere the person put it.
function restoreFocus() {
  if (!returnTo) return;
  const active = document.activeElement;
  // Nothing has the focus, or the drawer that is going still does, or the card
  // this already put it on. Anything else is where the person has since moved
  // it and is not to be taken away from them.
  const ours = !active || active === document.body
    || $('#drawer').contains(active) || active.dataset.id === String(returnTo);
  if (!ours) {
    returnTo = 0;
    return;
  }
  const card = $(`#board .card[data-id="${returnTo}"]`);
  if (card) card.focus();
}

// DAY is the shape the board this came from wrote, a day and a month with the
// year optional.
const DAY = /^(\d{1,2} [A-Za-z]{3,9}|[A-Za-z]{3,9} \d{1,2})( \d{4})?$/;

// isoDay is what goes into the due field. The card shows an ISO day and the
// placeholder asks for one, but somebody who types "24 Sep" means this year.
// Anything else goes up as typed and is refused by the server, which is the one
// place that decides what a due date may be.
function isoDay(typed) {
  const value = typed.trim().replace(/\s+/g, ' ');
  if (!value || /^\d{4}-\d{2}-\d{2}$/.test(value)) return value;
  // Only the shape the old board wrote is read. The browser's own parser will
  // find a year in anything at all and answer the first of January with it.
  const shape = DAY.exec(value);
  if (!shape) return value;
  const when = new Date(shape[2] ? value : value + ' ' + new Date().getFullYear());
  if (Number.isNaN(when.getTime())) return value;
  // A browser reads the thirty first of February as the second of March. A day
  // that does not come back as the one typed goes up as typed, so the server
  // refuses it and says so rather than quietly moving somebody's date.
  if (when.getDate() !== Number(shape[1].match(/\d{1,2}/)[0])) return value;
  const pad = (n) => String(n).padStart(2, '0');
  return `${when.getFullYear()}-${pad(when.getMonth() + 1)}-${pad(when.getDate())}`;
}

export function renderDrawer() {
  const drawer = $('#drawer');
  const card = state.cards.get(state.openCard);
  // One drawer, four things it can hold. A link, a file or the activity panel
  // takes it over, which is what clicking a row or the tab does.
  if (!card && !state.openLink && !state.openFile && !state.panel) {
    drawer.hidden = true;
    document.body.classList.remove('has-drawer');
    restoreFocus();
    return;
  }
  const top = drawer.scrollTop;
  // Read before the rebuild, because clearing the drawer drops the keyboard on
  // the body and the two would then be indistinguishable. Nothing focused, or
  // something in the drawer that is about to be replaced, both want the focus
  // put back; a control outside the drawer is where the person put it.
  const active = document.activeElement;
  const ours = !active || active === document.body || drawer.contains(active);
  clear(drawer);
  drawer.hidden = false;
  document.body.classList.add('has-drawer');

  // Opening a card, a link or a file supersedes the panel rather than fighting
  // it for the same column.
  if (state.panel && !card && !state.openLink && !state.openFile) {
    renderPanel(drawer);
    drawer.scrollTop = top;
    return;
  }

  if (state.openLink) {
    if (renderLinkDrawer(drawer)) { drawer.scrollTop = top; return; }
    state.openLink = null;
  }
  if (state.openFile) {
    if (renderFileDrawer(drawer)) { drawer.scrollTop = top; return; }
    state.openFile = null;
  }
  if (!card) {
    drawer.hidden = true;
    document.body.classList.remove('has-drawer');
    restoreFocus();
    return;
  }

  const column = state.columns.find((c) => c.id === card.column_id);
  const close = el('button', { class: 'x', type: 'button', text: 'Close', onclick: closeDrawer });
  drawer.append(el('div', { class: 'dh' },
    el('span', { class: 'mono', text: (column ? column.name : '') + ' · ' + (card.done_at ? 'done' : 'open') }),
    close));

  const heading = el('h2', { text: card.title, spellcheck: 'false' });
  if (canEdit()) {
    const edit = () => {
      if (heading.isContentEditable) return;
      hold(true);
      editable(heading, card.title, (value) => {
        hold(false);
        if (!value || value === card.title) { emit(); return; }
        versioned('card.title', card, { title: value });
      });
    };
    heading.addEventListener('click', edit);
    activate(heading, edit);
  }
  drawer.append(heading);
  add(drawer, [conflictBar(card, 'title')]);
  drawer.append(props(card));

  drawer.append(el('h4', { text: 'Description' }));
  drawer.append(description(card));
  add(drawer, [conflictBar(card, 'description_md')]);

  drawer.append(el('h4', { text: 'Checklist' }));
  drawer.append(checklist(card));

  drawer.append(el('h4', { text: 'Linked material' }));
  drawer.append(linked(card));

  drawer.append(el('h4', { text: 'Activity' }));
  drawer.append(activity(card));
  if (canEdit()) drawer.append(noteForm(card));
  if (canEdit() && state.can.delete) drawer.append(deleteCard(card));
  drawer.scrollTop = top;
  // Opened by a click or by Enter on the card, the keyboard comes with it. The
  // heading is the landing place where it does something, the close button
  // where it does not. As with the board, this acts only while the focus is on
  // nothing, so the render that follows the presence echo puts it back rather
  // than leaving it on the body, and a person who has since moved it keeps it.
  if (takeFocus === card.id) {
    if (ours) (canEdit() ? heading : close).focus();
    else takeFocus = 0;
  }
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
  if (canEdit()) {
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
    if (!canEdit()) return;
    const field = el('input', { class: 'inline', value: card.due_date || '', placeholder: 'YYYY-MM-DD' });
    due.replaceWith(field);
    hold(true);
    field.focus();
    let done = false;
    const finish = (save) => {
      if (done) return;
      done = true;
      hold(false);
      if (!save) { emit(); return; }
      send('card.due', { card: card.id, due: isoDay(field.value) }).catch((e) => say(e.message));
    };
    field.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') finish(true);
      if (e.key === 'Escape') finish(false);
    });
    field.addEventListener('blur', () => finish(true));
  });

  const question = el('select', {
    disabled: !canEdit(),
    onchange: (e) => send('card.question', { card: card.id, question: e.target.value }).catch((x) => say(x.message)),
  }, el('option', { value: '', text: 'none', selected: !card.question }));
  state.questions.forEach((q, i) => {
    question.append(el('option', {
      value: q, selected: q === card.question,
      text: q + ' · ' + (state.questionLabels[i] || ''),
    }));
  });

  const columns = el('select', {
    disabled: !canEdit(),
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
    el('dt', { text: 'State' }), el('dd', {}, canEdit()
      ? el('button', {
        class: 'lnk plain', type: 'button', text: card.done_at ? 'Done · reopen' : 'Open · mark done',
        onclick: () => send('card.done', { card: card.id, done: !card.done_at }).catch((e) => say(e.message)),
      })
      : el('span', { text: card.done_at ? 'Done' : 'Open' })));
}

function description(card) {
  const node = el('p', {
    class: 'desc', 'data-ph': 'Add a description. @ mentions notify people.',
    contenteditable: canEdit() ? 'true' : null, spellcheck: 'false',
  });
  add(node, [rendered(card.description_md || '')]);
  if (!canEdit()) return node;

  mentionable(node);
  node.addEventListener('focus', () => {
    hold(true);
    node.textContent = card.description_md || '';
  });
  node.addEventListener('blur', () => {
    hold(false);
    const value = node.textContent.trim();
    if (value === (card.description_md || '')) { emit(); return; }
    versioned('card.description', card, { text: value });
  });
  return node;
}

// versioned sends a text edit with the version it started from and, when the
// answer is a conflict, puts the choice in the page rather than in an alert.
// The choice is held in the state rather than hung off the node the edit was
// typed in: by the time the refusal arrives the drawer has been drawn again
// from the server's row and that node is no longer in the page. It is held by
// field, because a title and a description can each be waiting on one.
function versioned(cmd, card, args) {
  send(cmd, { card: card.id, base: card.version, ...args }).catch((err) => {
    if (!(err instanceof Conflict)) { say(err.message); emit(); return; }
    state.conflict[err.detail.field] = { card: card.id, cmd, args };
    emit();
  });
}

// conflictBar is the choice, drawn under the field it is about, or nothing when
// nothing on this card is waiting on one for that field. What theirs reads is
// taken from the row rather than from the refusal, because a row that moves on
// again while somebody is deciding would otherwise be quoted as it used to be.
function conflictBar(card, field) {
  const held = state.conflict[field];
  if (!held || held.card !== card.id) return null;
  const drop = () => { delete state.conflict[field]; emit(); };
  const bar = el('p', { class: 'notice bad' },
    'Somebody changed this while you were editing it. Theirs reads ',
    el('span', { class: 'mono', text: card[field] || '(nothing)' }), '. ');
  bar.append(el('button', {
    class: 'lnk', type: 'button', text: 'Keep mine',
    onclick: () => {
      // Sent again against the row as it stands now rather than against the
      // version the first refusal named. A row that has moved on once more
      // would be refused a second time and the typed text lost with nothing
      // on the screen to say so; this way the refusal comes back through here
      // and draws the choice again.
      drop();
      const live = state.cards.get(held.card);
      if (live) versioned(held.cmd, live, held.args);
    },
  }), ' ', el('button', {
    class: 'lnk plain', type: 'button', text: 'Take theirs', onclick: drop,
  }));
  return bar;
}

// deleteCard is the one thing the drawer could not do that the API always
// could. A researcher may not delete, so they are not offered it.
function deleteCard(card) {
  return el('div', { class: 'cf danger' }, el('button', {
    class: 'lnk del', type: 'button', text: 'Delete this card',
    onclick: () => ask(`Delete ${card.title}?`,
      'Its checklist, its notes and what is attached to it go with it. The record of the deletion stays in activity.',
      'Delete permanently').then((yes) => {
      if (!yes) return;
      send('card.delete', { card: card.id })
        .then(() => closeDrawer())
        .catch((err) => say(err.message));
    }),
  }));
}

function checklist(card) {
  const list = el('ul', { class: 'check' });
  for (const item of card.checklist || []) {
    list.append(el('li', { class: item.done ? 'd' : '' },
      el('label', {},
        el('input', {
          type: 'checkbox', checked: item.done, disabled: !canEdit(),
          onchange: (e) => send('checklist.toggle', { item: item.id, done: e.target.checked })
            .catch((x) => say(x.message)),
        }),
        ' ' + item.text),
      canEdit() && el('button', {
        class: 'lnk del quiet', type: 'button', text: 'remove',
        onclick: () => send('checklist.remove', { item: item.id }).catch((e) => say(e.message)),
      })));
  }
  if (canEdit()) {
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
    if (note.user_id === state.me && canEdit()) {
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
