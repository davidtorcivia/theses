// The document under the board: the tabs, the blocks, the source view, the
// history with its diff, and the one choice a conflict asks for.
//
// A block is markdown. Clicking one shows its source in a textarea sized to fit
// and a heading keeps the size it renders at; what is typed goes up every few
// hundred milliseconds as a block.set the server stores exactly as it was sent,
// with the version the editor started from, so somebody else's change in
// between is merged on the server or comes back as a choice.

import { $, el, add, clear, inline, say, editable, ask } from './dom.js';
import { state, user, byHandle, emit, hold, canEdit } from './state.js';
import { send, where, Conflict } from './net.js';
import { rebase } from './blocktext.js';

// The block this tab has open: its node and its textarea, and nothing else. The
// editor is a way of typing into an entry below, not a place anything is kept,
// so it can be given up the moment somebody looks away.
let editing = null;
let renameNext = 0;
// True while the page is being rebuilt. Taking the focused textarea out of the
// document fires a blur, and acting on that one would close the editor every
// time anybody else changed anything.
let rendering = false;

// work is every block this tab has something outstanding on, by block id. One
// entry holds all of it: the text as it stands here, the version it was last
// agreed at, the text last sent, whether one is in flight, the idle timer, and
// how the last save went. A block with an entry is a block whose text this tab
// is answerable for, whether or not anybody is looking at it.
//
//   text     what this tab holds, raw, exactly as it was typed
//   base     the version the next save is measured from
//   sent     the text the server last confirmed
//   first    when the first unsaved keystroke was, for the ceiling below
//   timer    the armed idle timer, zero for none
//   flight   true while a save of this block is in the air
//   status   'ok', or 'conflict' with theirs and version, or 'refused' with
//            reason: the two states a person has to answer before it saves
//
// ponytail: they live in this tab and nowhere else, so a reload leaves each
// block as the server last took it. The upgrade path is the one a refusal made
// offline already takes: keep the row in the outbox with its base text and the
// server's detail, and let the activity panel offer the same two answers.
const work = new Map();

// drawn is the node each block was last rendered as. The whole page is made
// again from the state on every applied event, and with somebody saving every
// second rebuilding blocks nobody touched would throw away their selection and
// their place on the page for nothing.
const drawn = new Map();

// idle is how long a pause in the typing waits before the text goes up, and
// ceiling is how long steady typing may hold it back.
const idle = 400;
const ceiling = 2000;

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

// blockAnywhere is a block in any of this proposition's documents. The block a
// save is about is not always in the open tab: one goes up after the person has
// moved on to another document, and the answer to a refusal has to find it
// wherever it is.
function blockAnywhere(id) {
  for (const d of documents()) {
    const block = (d.blocks || []).find((b) => b.id === id);
    if (block) return block;
  }
  return null;
}

// docWhere is what this tab tells the others it has open when it is not in a
// block: the document, so the tab is counted as here.
function docWhere() {
  const doc = current();
  return doc ? 'doc:' + doc.id : '';
}

// inside is who else has a block open, out of the presence the socket keeps.
// The shape of `where` is one string in two places and about to change, so the
// comparison lives here and nowhere else.
function inside(id) {
  return state.presence.filter((p) => p.where === 'block:' + id && p.id !== state.me);
}

// renderDocument returns the whole document area, head and all, for the board
// pane to append. It is called on every render, so the block being edited is
// carried over rather than rebuilt.
export function renderDocument() {
  const doc = current();
  if (doc) state.document = doc.id;
  const head = el('div', { class: 'ph doc-ph' }, tabs(doc), summary(doc));
  if (doc) {
    // The links sit together at the right. One auto margin each would share
    // the space between them and put History in the middle of the row.
    const links = el('div', { class: 'dlinks' },
      el('button', {
        class: 'lnk', type: 'button', id: 'dhistory', text: 'History',
        onclick: () => openHistory(doc),
      }),
      el('button', {
        class: 'lnk', type: 'button', id: 'docmode',
        text: state.docSource ? 'Rendered' : 'Source',
        onclick: () => { state.docSource = !state.docSource; emit(); },
      }));
    if (canEdit() && state.can.delete) links.append(deleteDocument(doc));
    head.append(links);
  }

  const body = el('div', { id: 'docwrap', class: state.docSource ? 'source' : '' });
  const rendered = el('div', { id: 'doc' });
  body.append(rendered, source(doc));
  if (!doc) {
    rendered.append(el('p', { class: 'empty', text: 'No document yet. The + above starts one.' }));
  } else {
    const ids = new Set();
    for (const b of doc.blocks || []) {
      ids.add(b.id);
      rendered.append(blockNode(b));
    }
    for (const id of drawn.keys()) if (!ids.has(id)) drawn.delete(id);
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
  prune();
  arrived();
}

// prune drops the text a block was holding for somebody once that block has
// gone. Nothing would ever draw it again and there is nothing left to answer
// it against, so it would sit in the map keeping the save line at "Not saved"
// for the rest of the session.
function prune() {
  let lost = false;
  for (const [id, w] of work) {
    if (blockAnywhere(id)) continue;
    clearTimeout(w.timer);
    work.delete(id);
    // Only worth saying when something went with it. A block somebody had
    // merely opened, or one whose text the server already has, is deleted like
    // any other block and the activity panel says who did it.
    if (busy(w)) lost = true;
  }
  if (lost) say('A block you had unsaved text in was deleted.');
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
    where(docWhere());
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
        where('doc:' + d.id);
        emit();
      },
    },
    // The save line speaks for the open document only, so a document with
    // something outstanding in it says so on its own tab. The open one has the
    // line itself and needs no dot.
    !on && held(d) ? el('span', { class: 'dot', title: 'Unsaved changes' }) : null));
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

// deleteDocument is the control the tabs never had. A researcher may not
// delete, so they are not offered it. The last document of a proposition goes
// like any other, because nothing on the server holds one back: the pane says
// the proposition has none and the + above starts the next one.
function deleteDocument(doc) {
  return el('button', {
    class: 'lnk del', type: 'button', id: 'docdel', text: 'Delete this document',
    onclick: () => ask(`Delete ${doc.name}?`,
      'Its blocks and the markdown file it is mirrored to go with it. The record of the deletion stays in activity.',
      'Delete permanently').then((yes) => {
      if (!yes) return;
      send('document.delete', { document: doc.id })
        .then(() => where(docWhere()))
        .catch((err) => say(err.message));
    }),
  });
}

function summary(doc) {
  const line = el('span', { id: 'dsum', class: 'mono' });
  if (!doc) {
    line.textContent = 'No document yet';
    return line;
  }
  const blocks = doc.blocks || [];
  const words = blocks.map((b) => b.text).join(' ').split(/\s+/).filter(Boolean).length;
  let last = null;
  for (const b of blocks) if (!last || b.updated_at > last.updated_at) last = b;
  let text = `${words} ${words === 1 ? 'word' : 'words'}`;
  if (last) {
    text += ' · edited ' + when(last.updated_at);
    text += last.updated_by ? ' by ' + user(last.updated_by).initials : ' on disk';
  }
  line.append(text + ' · ', el('span', { id: 'dsave', text: saveState() }));
  return line;
}

// status writes the save line in place. It changes on every keystroke, and a
// whole render for each one would take the caret out of the textarea.
function status() {
  const at = $('#dsave');
  if (at) at.textContent = saveState();
}

// entries is the outstanding work on one document's blocks. The save line and
// the tab dots are each about one document, so both ask this.
function entries(doc) {
  const out = [];
  for (const b of doc ? doc.blocks || [] : []) {
    const w = work.get(b.id);
    if (w) out.push(w);
  }
  return out;
}

// A block somebody has merely opened has an entry too, and that is not work
// outstanding: what counts is text the server has not taken, a save in the air,
// and an answer somebody owes. This is the same question the save line answers,
// so the two agree.
const busy = (w) => w.status !== 'ok' || w.flight || w.text !== w.sent;

const held = (doc) => entries(doc).some(busy);

// Saving is what is in the air, on a timer, or waiting for one of those. Text
// the server refused, or a block waiting on a choice, is not being saved and
// must not say it is; it comes first, because a question somebody has to answer
// outlasts a queue that is going up on its own.
//
// The queue itself says offline only when there is no network. With one, the
// same rows are a queue draining, which is the bar's "Catching up" and this
// line's "Saving…", and the two must not disagree at the foot of one page.
function saveState() {
  const here = entries(current());
  if (here.some((w) => w.status !== 'ok')) return 'Not saved';
  if (state.waitingHere > 0) {
    if (!navigator.onLine) return `Offline · ${state.waitingHere} kept on this device`;
    return 'Saving…';
  }
  if (here.some((w) => w.status === 'ok' && (w.flight || w.timer || w.text !== w.sent))) return 'Saving…';
  return 'Saved';
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
  // A block whose last save was refused draws what was written and the two
  // answers, whether or not anybody is in it. It is not an editor: it must not
  // take the caret from wherever the person went.
  const w = work.get(b.id);
  if (w && w.status !== 'ok') return clashed(b.id, w);

  const here = inside(b.id);
  // The text is in the key as well as the version, because a guess drawn before
  // the server has answered, and the undrawing of a refused one, both move the
  // text while the version stays where it was. So is whether this person may
  // edit, because that decides whether the node listens for a click at all and
  // it changes under them when a proposition is archived or restored.
  const key = `${canEdit()}:${b.version}:${here.map((p) => p.id).join(',')}:${b.text}`;
  const was = drawn.get(b.id);
  if (was && was.key === key) return was.node;

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
  drawn.set(b.id, { key, node });
  return node;
}

// body is the client renderer: the same markdown the mockup draws, built as
// nodes so that nothing anybody typed is ever parsed as markup.
//
// A block may hold a blank line, because a save made while somebody is typing
// stores the text as it was typed. It is drawn as it reads, one piece per blank
// line. This is a drawing, not a promise: what such a block becomes is the
// server's decision, and it makes it the next time an ordinary set, an import
// or the API touches the block.
function body(text) {
  if (!text.trim()) return [el('p', { class: 'empty', text: 'Empty. Click to write.' })];
  return text.split(/\n[ \t]*\n/).filter((part) => part.trim()).flatMap(piece);
}

function piece(text) {
  // A heading is its first line and nothing else, so whatever is under one is
  // drawn as what it is rather than swept into the heading.
  const [top, ...rest] = text.split('\n');
  const under = rest.join('\n');
  if (top.startsWith('# ') || top.startsWith('## ')) {
    const sub = top.startsWith('## ');
    return [add(el(sub ? 'h2' : 'h1'), [inline(top.slice(sub ? 3 : 2), byHandle)]),
      ...(under.trim() ? piece(under) : [])];
  }
  const lines = text.split('\n').filter((l) => l.trim());
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
  // Leaving the block this tab was in is the same whether a blur raised it or a
  // click somewhere else did. Nothing is held in a textarea, so nothing is lost
  // either way, but the block that is being left still sends what it has.
  if (editing) leave(editing.id);
  const b = blockOf(id);
  if (!b || !canEdit()) return;
  // The entry is made as the block opens, not when the first key is pressed:
  // it is what says which version this text came from, and somebody else's
  // change landing in between would otherwise look like agreement with it.
  // What this person wrote is what they see again.
  const w = entryOf(id);
  if (!w) return;
  const node = editor(id, w.text);
  const was = $(`#doc .blk[data-b="${id}"]`);
  if (was) was.replaceWith(node);
  editing.area.focus();
  editing.area.setSelectionRange(w.text.length, w.text.length);
}

// editor builds the block as a textarea and makes it the one this tab is in.
// It holds nothing: the text lives in the entry, and this is a way of typing
// into it.
function editor(id, text) {
  const area = el('textarea', { spellcheck: 'false', 'aria-label': 'This block as markdown' });
  area.value = text;
  const me = user(state.me);
  const node = el('div', { class: 'blk editing' + heading(text), 'data-b': id });
  node.append(area, el('span', { class: 'who me ' + me.colour, text: 'you' }));

  const fit = () => { area.style.height = 'auto'; area.style.height = area.scrollHeight + 'px'; };
  area.addEventListener('input', () => { typed(id); fit(); });
  area.addEventListener('keydown', (e) => {
    // Escape leaves the block. It stops here so that it does not also close
    // the drawer or the palette on its way up.
    if (e.key === 'Escape') { e.stopPropagation(); area.blur(); }
  });
  // A blur raised by the rebuild is not somebody leaving the block. The node is
  // put back by the same render and afterRender takes the caret with it.
  area.addEventListener('blur', () => { if (!rendering) leave(id); });
  editing = { id, node, area, fit };
  where('block:' + id);
  // The height is set after the node is in the page, because a detached
  // textarea has no scroll height to measure.
  queueMicrotask(fit);
  notice(id);
  return node;
}

// clashed is a block whose last save was refused. It shows what was written,
// what happened to it, and the two answers, and it is deliberately not an
// editor: the person may be in another block and the caret is theirs.
function clashed(id, w) {
  const area = el('textarea', { spellcheck: 'false', readonly: true,
    'aria-label': 'What you wrote in this block' });
  area.value = w.text;
  // Clicking the text is the third answer: go back in and write something else.
  // The listener is on the textarea rather than the block, so that a click on
  // one of the two buttons is not also a click into the editor.
  if (canEdit()) area.addEventListener('click', () => startEditing(id));
  const node = el('div', { class: 'blk clash' + heading(w.text), 'data-b': id },
    choice(id), area);
  queueMicrotask(() => { area.style.height = 'auto'; area.style.height = area.scrollHeight + 'px'; });
  return node;
}

// The save machine. One entry per block with something outstanding on it, one
// save of that block in the air at a time, and the editor holding nothing that
// is not already in the entry.

const openOn = (id) => Boolean(editing && editing.id === id);

// entryOf is the entry for a block, made from the block as it stands. base and
// sent are where this tab and the server agree the block was when the person
// opened it.
function entryOf(id) {
  let w = work.get(id);
  if (w) return w;
  const b = blockAnywhere(id);
  if (!b) return null;
  w = {
    text: b.text, base: b.version, sent: b.text,
    first: 0, timer: 0, flight: false, status: 'ok',
  };
  work.set(id, w);
  return w;
}

// finished drops an entry once there is nothing left in it: nobody in the
// block, nothing in the air, the server holding what this tab holds, and
// nothing to answer. Anything less and something goes with it. An entry dropped
// while a save is in flight takes the keystrokes typed during the round trip
// with it, and the ack arrives with nowhere to put what it brings back.
//
// It is the only thing that drops an entry, apart from the two answers that
// give the text up and the pruning of a block that is gone.
function finished(id) {
  const w = work.get(id);
  if (!w || openOn(id) || w.flight || w.text !== w.sent || w.status !== 'ok') return;
  work.delete(id);
  emit();
}

// typed is a keystroke in the open editor. The textarea is the text; nothing
// else about the entry is guessed from it.
function typed(id) {
  const w = entryOf(id);
  if (!w) return;
  w.text = editing.area.value;
  // A refusal stands until it is answered, and what is typed over it is kept
  // against that answer. Sending again on the next keystroke would be the same
  // text refused for the same reason every second of typing, with a line about
  // it each time; try it again and discard are the two ways out.
  if (w.status === 'refused') { status(); return; }
  if (w.status === 'conflict') {
    // Typing over a conflict is an answer to it: this text, from the version
    // they changed. Without moving the base it would be refused for ever.
    w.base = w.version;
    w.status = 'ok';
    notice(id);
  }
  arm(id, w);
}

// arm is the wait before a save: a pause in the typing sends it, and so does
// the ceiling, so somebody typing steadily has been saving all through it
// rather than at the end of it.
function arm(id, w) {
  clearTimeout(w.timer);
  w.timer = 0;
  if (w.status !== 'ok' || w.text === w.sent) {
    w.first = 0;
  } else {
    if (!w.first) w.first = Date.now();
    w.timer = setTimeout(() => { w.timer = 0; save(id); }, Math.min(idle, Math.max(0, w.first + ceiling - Date.now())));
  }
  status();
}

// save is the only save this editor makes: the text as it stands, stored as it
// stands. The server is told not to trim it or cut it into paragraphs, because
// doing either under somebody's caret is what this path exists to avoid.
//
// What that gives up: a block holds what was typed into it, a trailing space or
// a blank line included, until something else touches it. The API, MCP and the
// markdown import all still trim and cut, so an import of the mirror file
// normalises such a block as a side effect of reading it back.
function save(id) {
  const w = work.get(id);
  if (!w || w.flight || w.status !== 'ok' || w.text === w.sent) return;
  clearTimeout(w.timer);
  w.timer = 0;
  w.first = 0;
  const sent = w.text;
  const base = w.base;
  w.flight = true;
  send('block.set', { block: id, base, text: sent, whole: true })
    .then((ev) => acked(id, sent, base, ev))
    .catch((err) => refused(id, err));
  status();
}

// acked is a save coming back. Whether the server merged is a fact rather than
// a guess: the version it wrote over is the one in the ack's before, and if
// that is the base this save went with then what it stored is what was sent.
function acked(id, sent, base, ev) {
  const w = work.get(id);
  if (!w) return;
  w.flight = false;
  if (!ev) {
    // Queued with no socket, or the socket went while this was in the air. The
    // outbox holds it and replays it in order; base stays where it was, because
    // nothing has agreed to anything yet. Anything typed during the round trip
    // goes the same way, folded into the row already waiting there, and comes
    // back here once more with nothing left over.
    w.sent = sent;
    if (w.text !== w.sent) { save(id); return; }
    finished(id);
    status();
    return;
  }
  const merged = ev.before.version !== base;
  if (merged && !absorb(id, sent, ev)) return;
  w.base = ev.after.version;
  w.sent = merged ? ev.after.text : sent;
  if (w.text !== w.sent) { save(id); return; }
  finished(id);
  status();
}

// absorb folds what the server made of a block into the text this tab has, and
// into the textarea when somebody is looking at it. It answers false when the
// two cannot be folded together, having put the block into the conflict the
// person has to answer.
function absorb(id, sent, ev) {
  const w = work.get(id);
  const caret = openOn(id) ? editing.area.selectionStart : ev.after.text.length;
  const r = rebase(sent, ev.after.text, w.text, caret);
  if (r.conflict) {
    clash(id, ev.after.text, ev.after.version);
    return false;
  }
  w.text = r.text;
  if (openOn(id)) {
    editing.area.value = r.text;
    editing.area.setSelectionRange(r.caret, r.caret);
    editing.fit();
  }
  return true;
}

// refused is the server saying no. A conflict is two texts to choose between;
// anything else is a reason. Both stop this block saving until it is answered:
// typing answers a conflict, while a reason waits for try it again or discard,
// because the reason is usually still true a moment later. The block keeps the
// text and the notice meanwhile, on the editor or on the block itself once the
// editor has been given up.
function refused(id, err) {
  const w = work.get(id);
  if (!w) return;
  w.flight = false;
  clearTimeout(w.timer);
  w.timer = 0;
  w.first = 0;
  if (!blockAnywhere(id)) {
    // Somebody deleted the block while this was in the air. There is nothing
    // left to draw a refusal on and no answer anybody could give it.
    work.delete(id);
    say('A block you had unsaved text in was deleted.');
    emit();
    status();
    return;
  }
  if (err instanceof Conflict) {
    clash(id, err.detail.current, err.detail.version);
    return;
  }
  w.status = 'refused';
  w.reason = err.message;
  say(err.message);
  notice(id);
  emit();
  status();
}

// clash is the state a block goes into when two texts cannot be put together,
// whether the server said so or the rebase here could not either.
function clash(id, theirs, version) {
  const w = work.get(id);
  if (!w) return;
  w.status = 'conflict';
  w.theirs = theirs;
  w.version = version;
  clearTimeout(w.timer);
  w.timer = 0;
  w.first = 0;
  if (!openOn(id)) {
    say('Somebody changed a block you were writing in. It is waiting for your answer.');
  }
  notice(id);
  emit();
  status();
}

// notice keeps the line above the open editor in step with the entry, so that
// the block somebody is in says what happened to it without being rebuilt.
function notice(id) {
  if (!editing || editing.id !== id) return;
  const was = editing.node.querySelector('.notice');
  if (was) was.remove();
  const w = work.get(id);
  if (w && w.status !== 'ok') editing.node.prepend(choice(id));
}

// leave is the person going. The editor is given up at once, because nothing is
// kept in it, and what has not been saved goes now rather than waiting out the
// rest of the timer. A save already in the air keeps the entry: its ack sends
// whatever was typed during the round trip.
//
// A block waiting on an answer sends nothing, and that is what makes discard
// discard: the click that presses it blurs the textarea first, and a send from
// here would have written the text to the document on the way past.
function leave(id) {
  const w = work.get(id);
  if (openOn(id)) {
    if (w) w.text = editing.area.value;
    editing = null;
    where(docWhere());
    emit();
  }
  if (!w) return;
  clearTimeout(w.timer);
  w.timer = 0;
  w.first = 0;
  if (w.status === 'ok' && w.text !== w.sent) save(id);
  finished(id);
  status();
}

// arrived is somebody else's change to a block this tab has work on. A render
// follows every applied event, so the row moving past the version the entry is
// working from is where one is noticed.
function arrived() {
  for (const [id, w] of work) {
    if (w.flight) continue;
    const b = blockAnywhere(id);
    if (!b || b.version <= w.base) continue;
    if (b.text === w.text) {
      // The row says what this tab holds: this tab's own save arriving by
      // another road, which is what a page put away and brought back sees of
      // the one it sent on its way out. Agreeing with it is all there is to do,
      // whatever the last answer from the server was: a block the document
      // holds cannot still be a block that was not saved.
      w.base = b.version;
      w.sent = b.text;
      w.status = 'ok';
      delete w.theirs;
      delete w.version;
      delete w.reason;
      notice(id);
      finished(id);
      continue;
    }
    if (w.status !== 'ok') continue;
    if (w.text !== w.sent) {
      // Both texts are wanted. This one goes now against the base it was
      // working from, and the server's merge comes back through the ack.
      save(id);
      continue;
    }
    // Nothing of this person's is unsaved, so what arrived is taken as it
    // stands.
    w.base = b.version;
    w.sent = b.text;
    w.text = b.text;
    if (openOn(id)) {
      const caret = Math.min(editing.area.selectionStart, b.text.length);
      editing.area.value = b.text;
      editing.area.setSelectionRange(caret, caret);
      editing.fit();
    }
  }
}

// A tab closed mid sentence would otherwise lose it. These go whether or not a
// save is already in the air: there is no ack to wait for. pagehide also fires
// on a page that is only being put away, and these saves neither trim nor cut
// up, so the echo is exactly what this tab holds and arrived above takes it
// back without sending anything. The frame may not get away at all, which is
// the most this can promise.
//
// A block whose last save was refused goes too, from wherever the block has
// got to: the reason may have gone, and if it has not the refusal is the same
// one it already had. If the page comes back and the server took it, the row
// arrives holding what this tab holds, and arrived above takes the refusal off
// the block along with it. A block waiting on a conflict does not go: sending
// it would write over somebody else's words without anybody being asked, and
// nobody is there to ask.
addEventListener('pagehide', () => {
  if (editing) {
    const w = work.get(editing.id);
    if (w) w.text = editing.area.value;
  }
  for (const [id, w] of work) {
    if (w.status === 'conflict' || w.text === w.sent) continue;
    const base = w.status === 'refused' ? blockAnywhere(id)?.version ?? w.base : w.base;
    send('block.set', { block: id, base, text: w.text, whole: true }).catch(() => {});
  }
});

// choice is the notice above a block the server would not take: what happened,
// and the two answers. Both kinds come to the same pair, send what was written
// or let it go, so both are the same two buttons under different names.
function choice(id) {
  const w = work.get(id);
  const said = w.status === 'refused'
    ? [el('span', { text: 'That was not saved: ' + w.reason })]
    : [el('span', { text: 'Somebody changed this block while you were writing. It now reads: ' }),
      el('span', { class: 'theirs', text: w.theirs || '(nothing)' })];
  return el('div', { class: 'notice bad' }, said,
    el('span', { class: 'choices' },
      el('button', {
        class: 'lnk', type: 'button', text: w.status === 'refused' ? 'Try again' : 'Keep mine',
        onclick: () => resume(id, false),
      }),
      el('button', {
        class: 'lnk plain', type: 'button', text: w.status === 'refused' ? 'Discard' : 'Take theirs',
        onclick: () => resume(id, true),
      })));
}

// resume is the answer: send what was written again from wherever the block has
// got to, or give it up and let the block stand as the server has it.
function resume(id, give) {
  const w = work.get(id);
  const b = blockAnywhere(id);
  if (!w || !b) return;
  if (give) {
    // What they are given is what the block says now, not what it said when the
    // clash happened: it may have moved again since, and handing back the older
    // text would write over that third change without anybody being asked.
    clearTimeout(w.timer);
    work.delete(id);
    if (openOn(id)) {
      const fresh = entryOf(id);
      editing.area.value = fresh.text;
      editing.area.setSelectionRange(fresh.text.length, fresh.text.length);
      editing.fit();
      notice(id);
    }
    emit();
    status();
    return;
  }
  w.base = w.status === 'conflict' ? w.version : b.version;
  w.status = 'ok';
  notice(id);
  emit();
  save(id);
  status();
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
    el('p', { text: 'Changes are saved as you type. A version is kept when you ask, every two minutes while somebody is editing, and before anything is read back in from the markdown file.' }),
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
