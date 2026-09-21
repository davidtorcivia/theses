import { rememberTarget, copyTarget } from './anchors.js';
// The card drawer: assignees, due, question, column, state, description,
// checklist and the activity with its notes. Everything in it is a command, and
// the two text fields carry the version they started from.

import { $, el, clear, children, add, initials, inline, say, editable, ask } from './dom.js';
import { state, user, byHandle, proposition, emit, hold, canEdit, material, target, baseText, unresolvedCard } from './state.js';
import { send, where, newKey, count, chosen, resend, letGo, Conflict } from './net.js';
import { file as fileRefusal } from './offline.js';
import { openPicker, closePicker, mentionable } from './picker.js';
import { renderLinkDrawer, attachedLinks, host } from './links.js';
import { renderFileDrawer, attachedFiles, bytes } from './files.js';
import { renderPanel, closePanel } from './activity.js';
import { activate } from './keys.js';
import * as api from './api.js';

// takeFocus and returnTo carry the keyboard across one render. Opening a card
// puts the focus in the drawer and closing it puts the focus back on the card
// that was open, neither of which can be done where they are decided: the board
// and the drawer are both built again afterwards, and the nodes to focus do not
// exist until they are. Each is spent by the render that acts on it; holding
// one open would take the keyboard back off whoever had moved it since.
let takeFocus = 0;
let returnTo = 0;

export function closeDrawer() {
  if (unresolvedCard(state.openCard)) {
    say('Choose keep mine or take theirs before closing this card.');
    return false;
  }
  rememberTarget('', state.tab);
  const link = state.openLink;
  const file = state.openFile;
  const panel = state.panel;
  closePanel();
  returnTo = state.openCard;
  state.openCard = state.openLink = state.openFile = null;
  state.conflict = {};
  $('#drawer').hidden = true;
  document.body.classList.remove('has-drawer');
  where('');
  emit();
  if (link || file || panel) requestAnimationFrame(() => {
    const back = link ? $(`#llist .row[data-id="${link}"]`)
      : file ? $(`#flist .row[data-id="${file}"]`) : $('#activitytab');
    if (back) back.focus();
  });
  return true;
}

export function openCard(id, updateURL = true) {
  if (state.openCard !== id && unresolvedCard(state.openCard)) {
    say('Choose keep mine or take theirs before opening another card.');
    return false;
  }
  if (updateURL) rememberTarget('card', id);
  if (state.openCard !== id) {
    state.openCard = id;
    state.conflict = {};
    takeFocus = id;
    where('card:' + id);
  }
  state.openLink = state.openFile = null;
  emit();
  return true;
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
        'data-k': 'linkl' + link.id, text: link.title || host(link.url) }),
      el('span', { class: 'mono dim', text: ' ' + (link.kind || 'link') + ' · ' }),
      canEdit() ? el('button', { class: 'lnk del', type: 'button', text: 'Detach',
        'data-k': 'detachl' + link.id,
        onclick: () => detach('links', card.id, link.id) }) : null));
  }
  for (const file of attachedFiles(card.id)) {
    list.append(el('li', {},
      el('span', { text: file.name }),
      el('span', { class: 'mono dim', text: ' ' + bytes(file.size) + ' · ' }),
      canEdit() ? el('button', { class: 'lnk del', type: 'button', text: 'Detach',
        'data-k': 'detachf' + file.id,
        onclick: () => detach('files', card.id, file.id) }) : null));
  }
  if (!list.childElementCount) {
    list.append(el('li', { class: 'dim', text: 'Nothing linked yet.' }));
  }
  if (canEdit()) {
    list.append(el('li', {}, el('button', {
      class: 'lnk', type: 'button', text: '+ attach a link or a file', 'data-k': 'attach',
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

const rendered = (text) => inline(text, byHandle, proposition);

// restoreFocus puts the keyboard back on the card the drawer was showing, now
// that the board has been drawn again and that card is a node once more. Once,
// whether or not the card is still there: a card that has just been deleted has
// nothing to give the keyboard to, and the renders after this one keep it where
// it is by themselves.
function restoreFocus() {
  if (!returnTo) return;
  const card = $(`#board .card[data-id="${returnTo}"]`);
  returnTo = 0;
  if (card) card.focus();
}

// The drawer is built again from nothing on every render, so whatever had the
// keyboard goes with it. Every control it draws carries a key, and the rebuild
// puts the focus back on the one with the same key: the same thing docs.js does
// for the block somebody is typing in. A control with no key, and a focus
// outside the drawer, are both left alone.
function focusKey(drawer) {
  const active = document.activeElement;
  return active && drawer.contains(active) ? active.dataset.k || '' : '';
}

function refocus(drawer, key) {
  if (!key) return;
  const node = drawer.querySelector(`[data-k="${key}"]`);
  if (node) node.focus();
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
  if (state.openCard && !card) {
    state.openCard = null;
    history.replaceState(null, '', '#' + state.tab);
  }
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
  // the body and there would be nothing left to read it from.
  const key = focusKey(drawer);
  // Nothing holds the keyboard, or the card this drawer is for still does,
  // which is where the click or the Enter that opened it left it. Anything else
  // is somewhere a person has put it since and is not ours to take.
  const active = document.activeElement;
  const loose = !active || active === document.body
    || (active.dataset && active.dataset.id === String(state.openCard));
  if (state.openFile && !card && !state.openLink && !state.panel) {
    const nodes = [];
    if (renderFileDrawer({ append: (...items) => nodes.push(...items) })) {
      children(drawer, nodes);
      drawer.hidden = false;
      document.body.classList.add('has-drawer');
      drawer.scrollTop = top;
      return;
    }
  }
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
    history.replaceState(null, '', '#' + state.tab);
  }
  if (state.openFile) {
    if (renderFileDrawer(drawer)) { drawer.scrollTop = top; return; }
    state.openFile = null;
    history.replaceState(null, '', '#' + state.tab);
  }
  if (!card) {
    drawer.hidden = true;
    document.body.classList.remove('has-drawer');
    restoreFocus();
    return;
  }

  const column = state.columns.find((c) => c.id === card.column_id);
  const close = el('button', { class: 'x', type: 'button', text: 'Close', 'data-k': 'close', onclick: closeDrawer });
  drawer.append(el('div', { class: 'dh' },
    el('span', { class: 'mono', text: (column ? column.name : '') + ' · ' + (card.done_at ? 'done' : 'open') }),
    copyTarget(state.open, 'card', card.id), close));

  const heading = el('h2', { spellcheck: 'false', 'data-k': 'title' }, rendered(card.title));
  if (canEdit()) {
    mentionable(heading);
    const edit = (e) => {
      if (heading.isContentEditable || e?.target.closest('a')) return;
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
  // Opened by a click or by Enter on the card, the keyboard comes with it, once.
  // The heading is the landing place where it does something, the close button
  // where it does not. Any other render puts the focus back where it was.
  if (takeFocus === card.id) {
    takeFocus = 0;
    if (loose) (canEdit() ? heading : close).focus();
  } else {
    refocus(drawer, key);
  }
}

function props(card) {
  const who = el('dd', { class: 'who' });
  for (const id of card.assignees || []) {
    const person = user(id);
    who.append(el('button', {
      class: 'rm', type: 'button', title: 'Unassign ' + person.name, 'data-k': 'rm' + id,
      onclick: () => send('card.unassign', { card: card.id, user: id }).catch((e) => say(e.message)),
    }, initials(person)));
  }
  if (canEdit()) {
    who.append(el('button', {
      class: 'lnk', type: 'button', text: '+ assign', 'data-k': 'assign',
      onclick: (e) => {
        e.stopPropagation();
        openPicker(e.currentTarget, card.assignees || [], (person, on) =>
          send(on ? 'card.assign' : 'card.unassign', { card: card.id, user: person.id })
            .catch((err) => say(err.message)));
      },
    }));
  }

  const due = el('button', { class: 'lnk plain', type: 'button', 'data-k': 'due' },
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
    disabled: !canEdit(), 'data-k': 'question',
    onchange: (e) => send('card.question', { card: card.id, question: e.target.value }).catch((x) => say(x.message)),
  }, el('option', { value: '', text: 'none', selected: !card.question }));
  state.questions.forEach((q, i) => {
    question.append(el('option', {
      value: q, selected: q === card.question,
      text: q + ' · ' + (state.questionLabels[i] || ''),
    }));
  });

  const columns = el('select', {
    disabled: !canEdit(), 'data-k': 'column', 'aria-label': 'Move to column',
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
    el('dt', { text: 'Move to column' }), el('dd', {}, columns),
    el('dt', { text: 'State' }), el('dd', {}, canEdit()
      ? el('button', {
        class: 'lnk plain', type: 'button', 'data-k': 'state',
        text: card.done_at ? 'Done · reopen' : 'Open · mark done',
        onclick: () => send('card.done', { card: card.id, done: !card.done_at }).catch((e) => say(e.message)),
      })
      : el('span', { text: card.done_at ? 'Done' : 'Open' })));
}

function description(card) {
  const node = el('p', {
    class: 'desc', 'data-ph': 'Add a description. @ mentions notify people.', 'data-k': 'desc',
    contenteditable: canEdit() ? 'plaintext-only' : null, spellcheck: 'false',
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
async function versioned(cmd, card, args) {
  const full = { card: card.id, base: card.version, ...args };
  const idem = newKey();
  const key = target(cmd, full);
  const prior = state.refused.find((held) => held.key === key);
  const field = cmd === 'card.title' ? 'title' : 'description_md';
  const row = { proposition: state.open, me: state.me, cmd, args: full, idem,
    base: full.base, base_text: baseText(cmd, full) };
  try {
    await send(cmd, full, state.open, { key: idem });
    if (prior) await chosen(prior.n);
    delete state.conflict[field];
    return true;
  } catch (err) {
    if (!(err instanceof Conflict)) { say(err.message); emit(); return false; }
    const n = await fileRefusal(row, key, err.message, err.detail);
    if (n) {
      // Draw the choice immediately. count reads the same row back for this
      // and other tabs, but a failed read must not make it disappear here.
      state.refused = state.refused.filter((held) => held.n !== n);
      state.refused.push({ ...row, key, n, refused: err.message, detail: err.detail });
      delete state.conflict[err.detail.field];
      await count();
    } else {
      state.conflict[err.detail.field] = { card: card.id, cmd, args };
      say('This browser could not keep that conflict after the page closes. Choose an answer before leaving this card.');
    }
    emit();
    return false;
  }
}

// conflictBar is the choice, drawn under the field it is about, or nothing when
// nothing on this card is waiting on one for that field. What theirs reads is
// taken from the row rather than from the refusal, because a row that moves on
// again while somebody is deciding would otherwise be quoted as it used to be.
function conflictBar(card, field) {
  const saved = state.refused.find((row) => row.args && row.args.card === card.id
    && row.detail && row.detail.field === field);
  const local = state.conflict[field];
  const held = saved || (local && local.card === card.id ? { ...local, local: true } : null);
  if (!held) return null;
  const drop = async () => {
    if (held.local) {
      delete state.conflict[field];
      emit();
      return;
    }
    await letGo(held);
  };
  const bar = el('p', { class: 'notice bad' },
    'Somebody changed this while you were editing it. Theirs reads ',
    el('span', { class: 'mono', text: card[field] || '(nothing)' }), '. ');
  bar.append(el('button', {
    class: 'lnk', type: 'button', text: 'Keep mine', 'data-k': 'keep' + field,
    disabled: held.saving || null,
    onclick: async (e) => {
      // Sent again against the row as it stands now rather than against the
      // version the first refusal named. A row that has moved on once more
      // would be refused a second time and the typed text lost with nothing
      // on the screen to say so; this way the refusal comes back through here
      // and draws the choice again.
      const live = state.cards.get(card.id);
      if (!live) return;
      if (held.local) {
        const button = e.currentTarget;
        button.disabled = true;
        local.saving = true;
        const saved = await versioned(held.cmd, live, held.args);
        if (saved && state.conflict[field] === local) delete state.conflict[field];
        else { local.saving = false; button.disabled = false; }
        emit();
        return;
      }
      try {
        await resend(held, { ...held.args, base: live.version });
      } catch (err) {
        say(err.message);
      }
    },
  }), ' ', el('button', {
    class: 'lnk plain', type: 'button', text: 'Take theirs', 'data-k': 'theirs' + field,
    disabled: held.saving || null,
    onclick: () => drop().catch((err) => say(err.message)),
  }));
  return bar;
}

// deleteCard is the one thing the drawer could not do that the API always
// could. A researcher may not delete, so they are not offered it.
function deleteCard(card) {
  return el('div', { class: 'cf danger' }, el('button', {
    class: 'lnk del', type: 'button', text: 'Delete this card', 'data-k': 'delete',
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
          type: 'checkbox', checked: item.done, disabled: !canEdit(), 'data-k': 'chk' + item.id,
          onchange: (e) => send('checklist.toggle', { item: item.id, done: e.target.checked })
            .catch((x) => say(x.message)),
        }),
        ' ' + item.text),
      canEdit() && el('button', {
        class: 'lnk del quiet', type: 'button', text: 'remove', 'data-k': 'chkx' + item.id,
        onclick: () => send('checklist.remove', { item: item.id }).catch((e) => say(e.message)),
      })));
  }
  if (canEdit()) {
    const field = el('input', { class: 'newitem', placeholder: '+ item', 'data-k': 'newitem' });
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
        class: 'lnk del quiet', type: 'button', text: 'delete', 'data-k': 'notex' + note.id,
        onclick: () => send('comment.delete', { comment: note.id }).catch((e) => say(e.message)),
      }));
    }
    list.append(el('li', { 'data-comment': note.id, tabindex: '-1' }, initials(person), line));
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
  const field = el('textarea', { rows: '1', 'data-k': 'note',
    placeholder: 'Write a note. @ people or propositions. ⌘↵ posts.' });
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
    el('button', { class: 'lnk', type: 'button', text: 'Post', 'data-k': 'post', onclick: post }));
}
