// The document under the board: the tabs, the blocks, the source view, the
// history with its diff, and the one choice a conflict asks for.
//
// A block is markdown. Clicking one shows its source in a textarea sized to fit
// and a heading keeps the size it renders at; blurring sends block.set with the
// version the editor started from, so somebody else's change in between is
// merged on the server or comes back as a choice.

import { $, el, add, clear, inline, say, editable } from './dom.js';
import { state, user, byHandle, emit, hold, canEdit } from './state.js';
import { send, where, Conflict } from './net.js';

// The block this tab has open, and the choice a refused write is waiting on.
// Both are here rather than in state because nothing outside this module draws
// them and a re-render has to leave the editor exactly where it was.
let editing = null;
let conflict = null;
let renameNext = 0;
// True while the page is being rebuilt. Taking the focused textarea out of the
// document fires a blur, and acting on that one would close the editor every
// time anybody else changed anything.
let rendering = false;

export function documents() {
  return state.documents || [];
}

function current() {
  const list = documents();
  return list.find((d) => d.id === state.document) || list[0] || null;
}

function blockOf(id) {
  const doc = current();
  return doc ? (doc.blocks || []).find((b) => b.id === id) : null;
}

// docWhere is what this tab tells the others it has open when it is not in a
// block: the document, so the tab is counted as here.
function docWhere() {
  const doc = current();
  return doc ? 'doc:' + doc.id : '';
}

// renderDocument returns the whole document area, head and all, for the board
// pane to append. It is called on every render, so the block being edited is
// carried over rather than rebuilt.
export function renderDocument() {
  const doc = current();
  if (doc) state.document = doc.id;
  const head = el('div', { class: 'ph doc-ph' }, tabs(doc), summary(doc));
  if (doc) {
    // The two links sit together at the right. One auto margin each would
    // share the space between them and put History in the middle of the row.
    head.append(el('div', { class: 'dlinks' },
      el('button', {
        class: 'lnk', type: 'button', id: 'dhistory', text: 'History',
        onclick: () => openHistory(doc),
      }),
      el('button', {
        class: 'lnk', type: 'button', id: 'docmode',
        text: state.docSource ? 'Rendered' : 'Source',
        onclick: () => { state.docSource = !state.docSource; emit(); },
      })));
  }

  const body = el('div', { id: 'docwrap', class: state.docSource ? 'source' : '' });
  const rendered = el('div', { id: 'doc' });
  body.append(rendered, source(doc));
  if (!doc) {
    rendered.append(el('p', { class: 'empty', text: 'No document yet. The + above starts one.' }));
  } else {
    for (const b of doc.blocks || []) rendered.append(blockNode(b));
    if (!(doc.blocks || []).length) {
      rendered.append(el('p', { class: 'empty', text: 'Nothing in this document yet.' }));
    }
  }
  return [head, body];
}

// afterRender puts the caret back where it was. The whole pane is rebuilt on
// every applied event, which detaches the textarea and takes the focus with it,
// but the node itself is carried over so its text and its selection survive.
// beforeRender and afterRender bracket the rebuild. Everything on the page is
// made again from the state on every applied event, so the block being edited
// has to be carried across it: the node with its text and its selection is kept
// and put back, and the blur the rebuild raises on the way is ignored.
export function beforeRender() {
  rendering = true;
}

export function afterRender() {
  rendering = false;
  if (renameNext) {
    const tab = $(`#doctabs .dtab[data-d="${renameNext}"]`);
    renameNext = 0;
    if (tab) { renameTab(tab); return; }
  }
  if (!editing) return;
  if (!editing.area.isConnected) {
    // The block is not on the page any more, because somebody deleted it or
    // the document was switched out from under the editor. There is nothing
    // left to write to.
    editing = null;
    conflict = null;
    return;
  }
  const { selectionStart, selectionEnd } = editing.area;
  editing.area.focus();
  editing.area.setSelectionRange(selectionStart, selectionEnd);
}

function tabs(doc) {
  const bar = el('div', { id: 'doctabs', class: 'doctabs' });
  for (const d of documents()) {
    const on = doc && d.id === doc.id;
    bar.append(el('button', {
      type: 'button', class: 'dtab' + (on ? ' on' : ''), 'data-d': d.id, text: d.name,
      // Clicking the tab you are already on is how the mockup renames a
      // document, and the tab said nothing about it. The stylesheet hangs the
      // hover underline off this attribute, so the two arrive together.
      title: on && canEdit() ? 'Click again to rename' : null,
      onclick: (e) => {
        if (on) { if (canEdit()) renameTab(e.currentTarget); return; }
        state.document = d.id;
        state.docSource = false;
        conflict = null;
        where('doc:' + d.id);
        emit();
      },
    }));
  }
  if (canEdit()) {
    bar.append(el('button', {
      type: 'button', class: 'dtab new', id: 'docnew', title: 'New document', text: '+',
      onclick: () => {
        send('document.create', { proposition: state.open, title: 'Untitled' })
          .then((ev) => {
            state.document = ev.entity_id;
            state.docSource = false;
            renameNext = ev.entity_id;
            emit();
          })
          .catch((err) => say(err.message));
      },
    }));
  }
  return bar;
}

// renameTab turns the open tab into a one line editor, which is how the mockup
// renames a document: click the tab you are already on.
function renameTab(tab) {
  if (tab.isContentEditable) return;
  const id = Number(tab.dataset.d);
  const doc = documents().find((d) => d.id === id);
  if (!doc) return;
  hold(true);
  editable(tab, doc.name, (value) => {
    hold(false);
    if (value === null || value === doc.name || !value) { emit(); return; }
    send('document.rename', { document: id, title: value }).catch((err) => {
      say(err.message);
      emit();
    });
  });
}

function summary(doc) {
  if (!doc) return el('span', { id: 'dsum', class: 'mono', text: 'No document yet' });
  const blocks = doc.blocks || [];
  const words = blocks.map((b) => b.text).join(' ').split(/\s+/).filter(Boolean).length;
  let last = null;
  for (const b of blocks) if (!last || b.updated_at > last.updated_at) last = b;
  let text = `${words} ${words === 1 ? 'word' : 'words'}`;
  if (last) {
    text += ' · edited ' + when(last.updated_at);
    text += last.updated_by ? ' by ' + user(last.updated_by).initials : ' on disk';
  }
  return el('span', { id: 'dsum', class: 'mono', text });
}

function when(unix) {
  const at = new Date(unix * 1000);
  const days = Math.floor((Date.now() - at.getTime()) / 86400000);
  if (days < 1) return at.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (days < 7) return at.toLocaleDateString([], { weekday: 'short' });
  return at.toLocaleDateString([], { day: 'numeric', month: 'short' });
}

// The source view is the whole document as one markdown string, read only.
//
// ponytail: writing it back would mean splitting it on blank lines again and
// guessing which block each paragraph used to be, which is exactly what the
// markdown file on disk already does properly, ids and all. The upgrade path is
// to post this through the same importer the watcher uses.
function source(doc) {
  const area = el('textarea', { id: 'docsrc', spellcheck: 'false', readonly: true,
    'aria-label': 'This document as markdown' });
  area.value = doc ? (doc.blocks || []).map((b) => b.text).join('\n\n') : '';
  return area;
}

// Blocks.

function blockNode(b) {
  if (editing && editing.id === b.id) return editing.node;
  if (conflict && conflict.id === b.id) return editor(b.id, conflict.mine, conflict.version);

  const here = state.presence.filter((p) => p.where === 'block:' + b.id && p.id !== state.me);
  const node = el('div', {
    class: 'blk' + (here.length ? ' ' + user(here[0].id).colour : ''),
    'data-b': b.id, tabindex: '0',
  });
  add(node, body(b.text));
  for (const p of here) {
    const person = user(p.id);
    node.append(el('span', {
      class: 'who ' + person.colour, text: person.initials, title: person.name + ' is in this block',
    }));
  }
  if (canEdit()) {
    node.addEventListener('click', () => startEditing(b.id));
    node.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); startEditing(b.id); }
    });
  }
  return node;
}

// body is the client renderer: the same markdown the mockup draws, built as
// nodes so that nothing anybody typed is ever parsed as markup.
function body(text) {
  if (!text.trim()) return [el('p', { class: 'empty', text: 'Empty. Click to write.' })];
  if (text.startsWith('# ')) return [add(el('h1'), [inline(text.slice(2), byHandle)])];
  if (text.startsWith('## ')) return [add(el('h2'), [inline(text.slice(3), byHandle)])];
  const lines = text.split('\n');
  if (lines.every((l) => /^- /.test(l))) {
    return [add(el('ul'), lines.map((l) => add(el('li'), [inline(l.slice(2), byHandle)])))];
  }
  if (lines.every((l) => /^\d+\. /.test(l))) {
    return [add(el('ol'), lines.map((l) => add(el('li'), [inline(l.replace(/^\d+\. /, ''), byHandle)])))];
  }
  return [add(el('p'), [inline(text, byHandle)])];
}

// A heading keeps the size it renders at while it is being edited, which is
// what the h1 and h2 classes on an editing block are for.
function heading(text) {
  if (text.startsWith('# ')) return ' h1';
  if (text.startsWith('## ')) return ' h2';
  return '';
}

function startEditing(id) {
  if (editing && editing.id === id) return;
  const b = blockOf(id);
  if (!b || !canEdit()) return;
  const node = editor(id, b.text, b.version);
  const was = $(`#doc .blk[data-b="${id}"]`);
  if (was) was.replaceWith(node);
  editing.area.focus();
  editing.area.setSelectionRange(b.text.length, b.text.length);
}

// editor builds the block as a textarea and makes it the one this tab is in.
// base is the version the text came from, which is what a merge on the server
// is measured against.
function editor(id, text, base) {
  const area = el('textarea', { spellcheck: 'false', 'aria-label': 'This block as markdown' });
  area.value = text;
  const me = user(state.me);
  const node = el('div', { class: 'blk editing' + heading(text), 'data-b': id });
  if (conflict && conflict.id === id) node.append(choice(id));
  node.append(area, el('span', { class: 'who me ' + me.colour, text: 'you' }));

  const fit = () => { area.style.height = 'auto'; area.style.height = area.scrollHeight + 'px'; };
  area.addEventListener('input', fit);
  area.addEventListener('keydown', (e) => {
    // Escape leaves the block. It stops here so that it does not also close
    // the drawer or the palette on its way up.
    if (e.key === 'Escape') { e.stopPropagation(); area.blur(); }
  });
  // A blur raised by the rebuild is not somebody leaving the block. The node is
  // put back by the same render and afterRender takes the caret with it.
  area.addEventListener('blur', () => { if (!rendering) commit(id); });
  // opened is the text the editor started from, so that leaving a block
  // without typing in it writes nothing even when somebody else changed it in
  // the meantime; clashed is a block reopened on a refusal, where blurring
  // again means keep mine.
  editing = { id, node, area, base, opened: text, clashed: !!(conflict && conflict.id === id) };
  where('block:' + id);
  // The height is set after the node is in the page, because a detached
  // textarea has no scroll height to measure.
  queueMicrotask(fit);
  return node;
}

function commit(id) {
  if (!editing || editing.id !== id) return;
  const { area, base, opened, clashed } = editing;
  const text = area.value;
  editing = null;
  conflict = null;
  where(docWhere());

  if (!blockOf(id) || (text === opened && !clashed)) { emit(); return; }
  send('block.set', { block: id, base, text }).catch((err) => {
    if (err instanceof Conflict) {
      // The editor stays open with what this person wrote, above a line saying
      // what is there now and the two ways out of it.
      conflict = { id, mine: text, theirs: err.detail.current, version: err.detail.version };
      emit();
      return;
    }
    say(err.message);
    emit();
  });
  emit();
}

// choice is the notice a refused write puts above the block: who changed it,
// and keep mine or take theirs.
function choice(id) {
  const it = conflict;
  return el('div', { class: 'notice bad' },
    el('span', { text: 'Somebody changed this block while you were writing. It now reads: ' }),
    el('span', { class: 'theirs', text: it.theirs || '(nothing)' }),
    el('span', { class: 'choices' },
      el('button', {
        class: 'lnk', type: 'button', text: 'Keep mine',
        onclick: () => {
          const mine = it.mine;
          conflict = null;
          editing = null;
          send('block.set', { block: id, base: it.version, text: mine })
            .catch((err) => say(err.message))
            .finally(emit);
          emit();
        },
      }),
      el('button', {
        class: 'lnk plain', type: 'button', text: 'Take theirs',
        onclick: () => { conflict = null; editing = null; emit(); },
      })));
}

// History.

async function openHistory(doc) {
  let revisions = [];
  try {
    const res = await fetch(`/documents/${doc.id}/revisions`, { headers: { Accept: 'application/json' } });
    if (!res.ok) throw new Error('That history could not be read.');
    revisions = (await res.json()).revisions || [];
  } catch (err) {
    say(err.message);
    return;
  }

  const pane = el('div', { class: 'diff' });
  const list = el('ul', { class: 'hist' });
  const shown = [{ id: 0, reason: 'now', created_at: 0, markdown: (doc.blocks || []).map((b) => b.text).join('\n\n') }]
    .concat(revisions);
  shown.forEach((rev, i) => {
    const older = shown[i + 1];
    list.append(el('li', {},
      el('button', {
        class: 'lnk', type: 'button',
        text: rev.id ? `${label(rev.reason)} · ${when(rev.created_at)}` : 'As it is now',
        onclick: () => showDiff(pane, older ? older.markdown : '', rev.markdown),
      }),
      rev.created_by ? el('span', { class: 'mono', text: ' ' + user(rev.created_by).initials })
        : el('span', { class: 'mono', text: rev.id ? ' on disk' : '' })));
  });
  showDiff(pane, shown[1] ? shown[1].markdown : '', shown[0].markdown);

  const close = el('button', { class: 'lnk plain', type: 'button', text: 'Close' });
  const keep = el('button', { class: 'lnk', type: 'button', text: 'Save a version now' });
  const dialog = el('dialog', { class: 'history' },
    el('h3', { text: 'History of ' + doc.name }),
    el('p', { text: 'A version is kept when you ask, every ten minutes while somebody is editing, and before anything is read back in from the markdown file.' }),
    list, pane,
    el('div', { class: 'acts' }, keep, close));
  close.addEventListener('click', () => dialog.close());
  keep.addEventListener('click', () => {
    dialog.close();
    send('revision.create', { document: doc.id }).catch((err) => say(err.message));
  });
  dialog.addEventListener('close', () => dialog.remove());
  document.body.append(dialog);
  dialog.showModal();
}

function label(reason) {
  if (reason === 'pre-import') return 'Before a file was read in';
  if (reason === 'periodic') return 'While editing';
  return 'Saved';
}

function showDiff(pane, older, newer) {
  clear(pane);
  for (const [mark, line] of diff(older.split('\n'), newer.split('\n'))) {
    if (mark === ' ') pane.append(el('div', { class: 'same', text: line || ' ' }));
    else pane.append(el('div', { class: mark === '+' ? 'in' : 'out', text: mark + ' ' + line }));
  }
  if (!pane.firstChild) pane.append(el('div', { class: 'same', text: 'Nothing changed.' }));
}

// diff is a line level longest common subsequence, which is all a document of
// this size needs to show what a version changed.
//
// ponytail: it is O(n²) in lines and builds the whole table, which is nothing
// at a few hundred lines and would want the linear space form at a few
// thousand.
function diff(a, b) {
  const lcs = Array.from({ length: a.length + 1 }, () => new Array(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const out = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) { out.push([' ', a[i]]); i++; j++; }
    else if (lcs[i + 1][j] >= lcs[i][j + 1]) { out.push(['-', a[i]]); i++; }
    else { out.push(['+', b[j]]); j++; }
  }
  while (i < a.length) out.push(['-', a[i++]]);
  while (j < b.length) out.push(['+', b[j++]]);
  return out;
}
