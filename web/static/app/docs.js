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
import { send, live, where, onCarets, Conflict, Offline } from './net.js';
import { replace } from './api.js';
import { rebase, enter, chunks, carry, inFence, parseWhere, formatWhere } from './blocktext.js';
import { parts } from './blockparts.js';

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

// pending is the block a split is waiting on: the editor drawn where the new
// block will be, holding the text that went under the caret, before the server
// has given it an id. It is the one place text lives outside an entry, because
// there is no block to have an entry for yet, and it lasts exactly as long as
// the insert is in the air. The ack turns it into that block's editor and the
// text into that block's entry; a refusal puts the text back on the end of the
// block it was split from. One at a time: while one is in the air an Enter that
// would split is a plain newline. Which document it belongs to is held in the
// count below, where every insert in the air is.
let pending = null;

// inserting counts the block.insert commands this tab has in the air, by the
// document each is being made in. A block being made is work outstanding on
// that document that has no row to be counted on yet, and it is one document's
// work: the save line speaks for the open document and the tab dots each speak
// for their own.
const inserting = new Map();

// insert sends one and counts it while it is gone. Every block this editor
// makes goes through here, so there is one place that knows a document has a
// block on the way.
function insert(document, args) {
  inserting.set(document, (inserting.get(document) || 0) + 1);
  status();
  emit();
  return live('block.insert', { document, ...args }).finally(() => {
    const left = inserting.get(document) - 1;
    if (left > 0) inserting.set(document, left);
    else inserting.delete(document);
    status();
    emit();
  });
}

// Online is a socket and a network, and every key below that adds or removes a
// block asks for both. block.insert and block.delete are not drawn until the
// server answers, so a split made with nothing to answer it would take what was
// typed off the page and leave it nowhere anybody could see it. With no
// connection those keys do what a plain textarea does: an Enter that would
// split is a newline, a paste is one block, Backspace at nought does nothing.
// Enter inside a list is typing into a block that exists, so it works either way.
const online = () => state.connected && navigator.onLine;

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

// inside is who else has a block open, out of the presence the socket keeps,
// and where in it each of them is standing. parseWhere reads the shape of the
// string and this is the one place that asks it about a block.
function inside(id) {
  const out = [];
  for (const p of state.presence) {
    if (p.id === state.me) continue;
    const at = parseWhere(p.where);
    if (at && at.block === id) out.push({ id: p.id, version: at.version, start: at.start, end: at.end });
  }
  return out;
}

// seen is the last caret this tab actually drew for somebody, by block and
// then by person. An offset only means something against a version, so one
// naming a version the block is not at cannot be placed: their save or their
// caret arrived first, and putting them somewhere wrong is worse than leaving
// them where they were last right. Everything that writes it goes through
// carets below, which keeps only the people who are in the block now, so
// somebody leaving takes their entry with them and a block that goes takes its
// whole map in the prune beside drawn.
const seen = new Map();

// carets is where the other people in a block are, in the text this tab is
// about to draw. version is the version that text is at, and move carries one
// of their offsets into it: a block nobody here is typing in is drawn at the
// row's own version, where an offset lands as it was sent, and the block this
// tab is in is drawn at its entry's base with whatever has been typed since
// carried through.
function carets(id, here, version, move) {
  const was = seen.get(id);
  const now = new Map();
  const out = [];
  for (const p of here) {
    let at = was ? was.get(p.id) : null;
    if (p.version && p.version === version) at = { start: move(p.start), end: move(p.end) };
    if (!at) continue;
    now.set(p.id, at);
    const person = user(p.id);
    out.push({ ...at, colour: person.colour, initials: person.initials });
  }
  if (now.size) seen.set(id, now);
  else seen.delete(id);
  return out;
}

// renderDocument returns the whole document area, head and all, for the board
// pane to append. It is called on every render, so the block being edited is
// carried over rather than rebuilt.
export function renderDocument() {
  const doc = current();
  if (doc) state.document = doc.id;
  if (doc && state.docSource) ensureSource(doc);
  const head = el('div', { class: 'ph doc-ph' }, tabs(doc), summary(doc));
  if (doc) {
    // The links sit together at the right. One auto margin each would share
    // the space between them and put History in the middle of the row.
    const links = el('div', { class: 'dlinks' },
      el('button', {
        class: 'lnk', type: 'button', id: 'dhistory', text: 'History',
        onclick: () => openHistory(doc),
      }),
      // Save stands between the two, next to the toggle it belongs to, and is
      // there only while the markdown is open to somebody who may write it.
      state.docSource && canEdit() && sourceOf(doc) ? saveButton(doc) : null,
      el('button', {
        class: 'lnk', type: 'button', id: 'docmode',
        text: state.docSource ? 'Rendered' : 'Source',
        onclick: () => {
          // Going back to the rendered document keeps whatever has been
          // written in the markdown: it is this tab's for the rest of the
          // session, and Source brings it back with the blocks it was opened
          // on, so nothing has to be asked before the view changes.
          if (state.docSource || openSource(doc)) {
            state.docSource = !state.docSource;
            emit();
          }
        },
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
      // The provisional editor is put back directly under the block it came
      // out of, on every render, so the page reads as it will once the server
      // has answered. It has no block id, so nothing else here sees it.
      if (pending && pending.anchor === b.id) rendered.append(pending.ed.node);
    }
    for (const id of drawn.keys()) if (!ids.has(id)) drawn.delete(id);
    // seen goes the same way, by its own keys rather than drawn's: the block
    // this tab is typing in is never in drawn, and its mirror writes into seen
    // like every other block. This is where a deleted block and a document
    // switched out from under the page both drop what was drawn in them.
    for (const id of seen.keys()) if (!ids.has(id)) seen.delete(id);
    if (!(doc.blocks || []).length && !canEdit()) {
      rendered.append(el('p', { class: 'empty', text: 'Nothing in this document yet.' }));
    }
    // The + is the way to start an empty document as well as the way to add to
    // a full one, so it stands in for the line above rather than sitting under
    // it.
    if (canEdit()) rendered.append(addBlock(doc));
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
  // Where the person is in the markdown they are writing. The textarea itself
  // is carried across the rebuild with its text, but taking it out of the page
  // drops the focus and can move what is scrolled into view. Only the open
  // document's can be on the page, so it is the only one asked.
  const src = sourceOf(current());
  if (src && src.area.isConnected) {
    src.at = [src.area.selectionStart, src.area.selectionEnd];
    src.scroll = src.area.scrollTop;
    src.focused = document.activeElement === src.area;
  }
}

// prune drops the text a block was holding for somebody once that block has
// gone. Nothing would ever draw it again and there is nothing left to answer
// it against, so it would sit in the map keeping the save line at "Not saved"
// for the rest of the session. The markdown somebody was writing goes the same
// way when its document does, which is the one thing that ever takes a session
// out of the map besides a save that went through.
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
  for (const id of sources.keys()) if (!documents().some((d) => d.id === id)) sources.delete(id);
}

export function afterRender() {
  rendering = false;
  const src = sourceOf(current());
  if (src && src.area.isConnected) {
    // The scroll goes back whether or not they were in it: reading the
    // markdown while somebody else writes is a page that must not jump.
    src.area.scrollTop = src.scroll;
    if (src.focused) {
      src.area.focus();
      src.area.setSelectionRange(src.at[0], src.at[1]);
      src.area.scrollTop = src.scroll;
    }
  }
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
  // A textarea measured while it was out of the page has no height to measure,
  // which is what an editor opened on a block this render is the first to draw
  // has. This is the first moment it can be sized.
  // fit draws the mirror as well, so presence arriving, which arrives as a
  // render like everything else, is what moves somebody else's caret in the
  // block this tab is typing in. And this is where the caret this tab is
  // telling the others about catches up with whatever has just happened to the
  // block under it, through the throttle, because a render is exactly what the
  // last caret this tab sent has just caused.
  editing.fit();
  moveCaret();
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

// A block on its way into a document is outstanding on that document too, so
// its tab carries the dot while the insert is in the air.
const held = (doc) => Boolean(doc && inserting.has(doc.id)) || entries(doc).some(busy);

// Saving is what is in the air, on a timer, or waiting for one of those. Text
// the server refused, or a block waiting on a choice, is not being saved and
// must not say it is; it comes first, because a question somebody has to answer
// outlasts a queue that is going up on its own.
//
// The queue itself says offline only when there is no network. With one, the
// same rows are a queue draining, which is the bar's "Catching up" and this
// line's "Saving…", and the two must not disagree at the foot of one page.
function saveState() {
  const doc = current();
  const here = entries(doc);
  if (here.some((w) => w.status !== 'ok')) return 'Not saved';
  if (state.waitingHere > 0) {
    if (!navigator.onLine) return `Offline · ${state.waitingHere} kept on this device`;
    return 'Saving…';
  }
  // A block this document is waiting on is text this tab is answerable for
  // that has no entry to be counted in yet. A block on its way into another
  // document is that document's line to say, not this one's.
  if (doc && inserting.has(doc.id)) return 'Saving…';
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

// The source view is the whole document as one markdown string. A viewer reads
// it; anybody who may write edits it and saves it back through the same
// importer the markdown file on disk goes through, which is what keeps the
// block a paragraph came out of when it comes back unchanged.
//
// sources is one editing session per document, by document id: the blocks the
// markdown was opened on with the version each was at, the text as it stands
// here, and the textarea it is being typed into. The base is what the server
// lines the paragraphs up against, so it is taken once, when the markdown is
// opened, and never moved by anything that happens afterwards; what has been
// typed lives in text, not only in the textarea, the way a block's does in its
// entry.
//
// One per document rather than one for the tab, because the tabs above the
// document are a click apart: a session that followed whichever tab was open
// would leave the markdown of the document somebody was writing to be thrown
// away by the next document they looked at. Each outlives the toggle and the
// tab switch, so coming back to a document finds the same text against the same
// base and nothing has to be asked. None of them outlives the page: a reload
// leaves every document as the server last took it, which is what an unsaved
// block does too.
const sources = new Map();

// sourceOf is the session for a document, or null, and the only reader of the
// map: a session is never asked for by anything but the document it belongs to,
// so one cannot be drawn or saved under another.
const sourceOf = (doc) => (doc && sources.get(doc.id)) || null;

// fresh is the document of that id as the state holds it now. A session is
// taken from it rather than from the document a render closed over, because
// leave sends and renders before the markdown is read, and because the person
// may have moved to another tab while a dialog was open: current() would then
// be a different document and its markdown would be thrown away.
const fresh = (id) => documents().find((d) => d.id === id) || null;

const markdownOf = (doc) => (doc.blocks || []).map((b) => b.text).join('\n\n');

function source(doc) {
  const wrap = el('div', { id: 'docsource' });
  const src = sourceOf(doc);
  if (src && canEdit()) {
    // The textarea is carried from render to render rather than made again,
    // because somebody else saving a block is a render like any other and it
    // must not take the caret, the scroll or the words out from under the
    // person writing. beforeRender and afterRender put the caret and the
    // scroll back across the move.
    if (moved(doc, src)) {
      wrap.append(el('p', { class: 'notice',
        text: 'This document has changed since you opened its markdown. Saving merges what you have written into theirs, paragraph by paragraph.' }));
    }
    wrap.append(src.area);
    return wrap;
  }
  const area = el('textarea', { id: 'docsrc', spellcheck: 'false', readonly: true,
    'aria-label': 'This document as markdown' });
  area.value = doc ? markdownOf(doc) : '';
  wrap.append(area);
  return wrap;
}

// moved reports the document standing somewhere other than where the open
// markdown was taken from, which is the note above the textarea.
function moved(doc, src) {
  const now = doc.blocks || [];
  return now.length !== src.base.length
    || now.some((b, i) => b.id !== src.base[i].id || b.version !== src.base[i].version);
}

// openSource is the Source link pressed. It answers whether the view may open.
//
// The base has to be what the server holds, so the block this tab was in is
// left the ordinary way first and anything still outstanding refuses the whole
// thing: markdown taken while a block's text was on its way up would carry the
// old paragraph and write it back over the new one. A viewer opens the read
// only view and has no session to make.
function openSource(doc) {
  if (!doc || !canEdit()) return true;
  if (sourceOf(doc)) return true;
  // A provisional editor names no block and is left where it is: its insert is
  // in the air, which the count below refuses on anyway.
  if (editing && editing.id) leave(editing.id);
  if (entries(doc).some((w) => w.status !== 'ok')) {
    say('Answer the block that is waiting on you before you open the markdown.');
    return false;
  }
  if (held(doc)) {
    say('Something here is still saving. Open the markdown again in a moment.');
    return false;
  }
  const now = fresh(doc.id);
  if (!now) {
    say('That document is no longer there.');
    return false;
  }
  reopen(now);
  return true;
}

// ensureSource is the render's half of openSource, for a document drawn while
// the markdown is showing that has no session of its own: the toggle is one
// flag for the pane rather than one per document, so without this a document
// that came up under it would be markdown nobody could save. Nothing was
// pressed, so there is no editor to leave and nothing to say; a document with
// work outstanding keeps the read only view until it settles, which is the same
// rule openSource states out loud.
function ensureSource(doc) {
  if (!canEdit() || sourceOf(doc)) return;
  if (held(doc) || entries(doc).some((w) => w.status !== 'ok')) return;
  reopen(doc);
}

// reopen takes the markdown and the base from the document as it stands now,
// which is both opening the view and the answer to a save that could not take
// everything.
function reopen(doc) {
  const area = el('textarea', { id: 'docsrc', spellcheck: 'false',
    'aria-label': 'This document as markdown' });
  area.value = markdownOf(doc);
  const src = {
    base: (doc.blocks || []).map((b) => ({ id: b.id, version: b.version })),
    text: area.value,
    area,
    at: [0, 0],
    scroll: 0,
    focused: false,
    key: '',
  };
  sources.set(doc.id, src);
  area.addEventListener('input', () => { src.text = area.value; });
}

// saveButton writes the markdown back. It is a request rather than a command,
// with an answer of its own, so it does not go over the socket and there is
// nothing to queue: with no connection it says so and waits.
function saveButton(doc) {
  const off = !online();
  return el('button', {
    class: 'lnk', type: 'button', id: 'docsave', text: 'Save',
    disabled: off || null,
    title: off
      ? 'Writing the markdown back needs the server, and this device has no connection.'
      : 'Write this markdown back into the document',
    onclick: (e) => {
      const button = e.currentTarget;
      button.disabled = true;
      // The button is re-enabled here rather than on a render, because a save
      // that changed nothing causes none, and a second press while the first
      // is in the air would be the same paragraphs written twice.
      writeSource(doc).finally(() => { button.disabled = false; });
    },
  });
}

// writeSource is the only thing that sends the markdown, and only a press
// reaches it: nothing here saves on a timer, on leaving, or on a render.
//
// The base the answer comes back with is what the text now stands on, block by
// block, and it replaces the one the session was opened with. That is what
// makes a second press write nothing: every paragraph is already on the block
// the server named, at the version it named. Where they differ, which is a
// paragraph somebody else changed too, the version named is theirs, so the next
// press writes this person's paragraph over it. That is what keep mine means
// here as everywhere else, and the line below says so before they press.
async function writeSource(doc) {
  const src = sourceOf(doc);
  if (!src) return;
  // One key per attempt, kept only while no answer has come back at all. A
  // request that was never answered may or may not have been applied, and
  // sending it again under the same key is how the server says which; anything
  // it does answer, refusal included, leaves nothing applied that a fresh key
  // would apply twice.
  src.key = src.key || crypto.randomUUID();
  let answer;
  try {
    answer = await replace(`/documents/${doc.id}/source`, { base: src.base, text: src.text },
      { 'Idempotency-Key': src.key });
  } catch (err) {
    if (err.status !== 0) src.key = '';
    say(err.message);
    return;
  }
  src.key = '';
  if (answer.replayed) {
    // The attempt that was never answered had in fact gone through. Nothing
    // remembers what it answered, so the markdown is read again from the
    // document, which is what it wrote.
    //
    // ponytail: what that loses is this person's wording of any paragraph the
    // first attempt could not take, because the conflict list went with the
    // answer nobody saw and reading again replaces the text with the
    // document's, where such a paragraph reads as the other person left it.
    // The upgrade is remembering the first answer beside the key.
    say('That save had already gone through. This is the document as it now reads.');
    const back = fresh(doc.id);
    if (back) {
      reopen(back);
      emit();
    }
    return;
  }
  src.base = answer.base || src.base;
  const left = (answer.conflicts || []).length;
  if (!left) {
    sources.delete(doc.id);
    state.docSource = false;
    emit();
    return;
  }
  // The rest of it went in. What is left is theirs, and the two answers are
  // take the document as it now reads, which throws this text away, or stay
  // here with what was written.
  const one = left === 1;
  const yes = await ask(
    `${left} ${one ? 'paragraph was' : 'paragraphs were'} left as ${one ? 'it is' : 'they are'}, because somebody else changed ${one ? 'it' : 'them'} while you were writing.`,
    `Everything else you wrote went in. Saving again writes ${one ? 'your paragraph' : 'your paragraphs'} over theirs. To take what they wrote instead, read the markdown again, which throws away what is in front of you.`,
    'Read it again');
  const now = yes && fresh(doc.id);
  if (now) {
    reopen(now);
    emit();
  }
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
  // it changes under them when a proposition is archived or restored. So is
  // each caret, so that somebody moving about a block rebuilds that one block
  // and nothing else on the page.
  const key = `${canEdit()}:${b.version}:${here.map((p) => `${p.id}@${p.version}:${p.start}:${p.end}`).join(',')}:${b.text}`;
  const was = drawn.get(b.id);
  if (was && was.key === key) return was.node;

  const at = carets(b.id, here, b.version, (n) => Math.min(n, b.text.length));
  const node = el('div', {
    class: 'blk' + (here.length ? heading(b.text) + ' ' + user(here[0].id).colour : ''),
    'data-b': b.id, tabindex: '0',
  });
  // A block somebody else is standing in is drawn as its source rather than
  // rendered: their offset is counted in the markdown, and there is no honest
  // place to stand in a rendering of it. It goes back to rendered markdown the
  // moment they leave.
  if (here.length) node.append(add(el('div', { class: 'src' }), raw(b.text, at)));
  else add(node, body(b.text));
  for (const p of here) {
    const person = user(p.id);
    node.append(el('span', {
      class: 'who ' + person.colour, text: person.initials, title: person.name + ' is in this block',
    }));
  }
  if (canEdit()) {
    node.addEventListener('click', (e) => { if (!onLink(e)) startEditing(b.id); });
    node.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !onLink(e)) { e.preventDefault(); startEditing(b.id); }
    });
  }
  drawn.set(b.id, { key, node });
  return node;
}

// onLink is whether what was clicked, or what the Enter was pressed on, is a
// link in the block rather than the block itself. Following a link is not a way
// of asking to edit the words around it: the click would open the editor behind
// the navigation, which is invisible in a tab that leaves and is an editor left
// standing in one that does not, a link opened in a new tab or with a modifier
// held. The keyboard needs it more: the block's Enter calls preventDefault, so
// without this a link reached by the Tab key could not be followed at all.
//
// It only ever matches in a rendered block. raw(), which draws a block somebody
// else is standing in, builds text and spans and no links at all, so such a
// block opens on a click wherever it is clicked, as it did.
const onLink = (e) => !!e.target.closest('a');

// body is the client renderer: the same markdown the mockup draws, built as
// nodes so that nothing anybody typed is ever parsed as markup. What the text
// is made of is blockparts.js, which has no page in it and is tested on its
// own; this is the drawing of it.
//
// A block may hold a blank line, because a save made while somebody is typing
// stores the text as it was typed. It is drawn as it reads, one piece per blank
// line. This is a drawing, not a promise: what such a block becomes is the
// server's decision, and it makes it the next time an ordinary set, an import
// or the API touches the block.
function body(text) {
  if (!text.trim()) return [el('p', { class: 'empty', text: 'Empty. Click to write.' })];
  try {
    return parts(text).map(drawPart);
  } catch (e) {
    // The document is drawn inside the board's pane, so a renderer that threw
    // over one block would take the whole proposition off the page for as long
    // as the block said what it said. The words are drawn as one paragraph
    // instead, which is what the block would read as with no markdown at all.
    //
    // The console is the one place this can go, and the only call to it in the
    // app: a fault in the renderer is nothing the reader can answer, so it is
    // not a line under say(), and it must not go nowhere either, or all that
    // would be left of it is a block that reads oddly.
    console.error('this block could not be read as markdown', e);
    return [add(el('p'), [inline(text, byHandle)])];
  }
}

function drawPart(part) {
  switch (part.kind) {
    case 'h1': case 'h2': case 'h3':
      return add(el(part.kind), [inline(part.text, byHandle)]);
    case 'ul': case 'ol':
      return add(el(part.kind), part.items.map((item) => add(el('li'), [inline(item, byHandle)])));
    case 'quote':
      return add(el('blockquote'), part.paragraphs.map((said) => add(el('p'), [inline(said, byHandle)])));
    // Code is text and nothing else: no inline markdown in it and no
    // highlighting, so its info string is left in the markdown a click shows.
    case 'code':
      return scrolls(el('pre', {}, el('code', { text: part.text })), 'Code block');
    case 'table':
      return scrolls(grid(part), 'Table');
    default:
      return add(el('p'), [inline(part.text, byHandle)]);
  }
}

// Code and a table are the two things in a document wider than the pane it is
// read in. Each is wrapped in a box that scrolls on its own, so a phone moves
// the code sideways rather than the page. The box takes the keyboard, because
// a scroller nothing can focus cannot be scrolled without a pointer.
function scrolls(node, label) {
  return el('div', { class: 'scroll', tabindex: '0', role: 'region', 'aria-label': label }, node);
}

// The alignment of a column is a class: the CSP has no unsafe-inline in
// style-src, and there are three of them.
const ALIGN = { l: 'al-l', c: 'al-c', r: 'al-r' };

function grid(part) {
  const cell = (tag, text, i) => add(el(tag, {
    class: ALIGN[part.align[i]] || null, scope: tag === 'th' ? 'col' : null,
  }), [inline(text, byHandle)]);
  return el('table', {},
    el('thead', {}, add(el('tr'), part.head.map((text, i) => cell('th', text, i)))),
    add(el('tbody'), part.rows.map((row) => add(el('tr'), row.map((text, i) => cell('td', text, i))))));
}

// raw is a block drawn as the markdown the people in it are looking at, with
// their carets standing in it. The stylesheet lays it out by the same rule as
// the editor's textarea, so an offset counted in their text lands on the same
// character here. Text and spans only, like everything else on this page.
//
// The line break on the end is the one a textarea draws and pre-wrap does not:
// a segment break at the end of a block makes no line box of its own, so
// without it an empty block would have no height at all and a caret on the
// line after the last one would have nowhere to stand.
function raw(text, here) {
  // A caret kept from an older version of the block can name an offset this
  // text is too short for. It is drawn at the end rather than dropped: they
  // are still in the block, and the marker beside it says so either way.
  const marks = here.map((c) => ({ ...c, start: Math.min(c.start, text.length), end: Math.min(c.end, text.length) }));
  const cuts = new Set([0, text.length]);
  for (const c of marks) { cuts.add(c.start); cuts.add(c.end); }
  const points = [...cuts].sort((a, b) => a - b);
  const out = [];
  for (let i = 0; i + 1 < points.length; i++) {
    const from = points[i];
    const to = points[i + 1];
    for (const c of marks) if (c.start === from) out.push(flag(c));
    // The first of two people whose selections overlap colours the run they
    // share. One tint over another would read as a third colour nobody is.
    const over = marks.find((c) => c.end > c.start && c.start <= from && c.end >= to);
    out.push(over ? el('span', { class: 'tint ' + over.colour, text: text.slice(from, to) }) : text.slice(from, to));
  }
  for (const c of marks) if (c.start === text.length) out.push(flag(c));
  // A block with nothing in it still has a line to stand on, which an empty
  // textarea draws and a run of no characters does not. The zero width space
  // goes after the caret at nought, so somebody standing at the start of an
  // empty block is still drawn at the start of it.
  if (!text) out.push('​');
  out.push('\n');
  return out;
}

// flag is one person standing at an offset: a bar in their colour with their
// initials above it. Both are drawn out of the flow, so the span itself takes
// up no room and the text is laid out as though it were not there. The block's
// title is what says out loud who is in it; this is the picture of it.
function flag(c) {
  return el('span', { class: 'caret ' + c.colour, 'aria-hidden': 'true' },
    el('span', { class: 'flag', text: c.initials }));
}

// A heading keeps the size it renders at while it is being edited, which is
// what the h1, h2 and h3 classes on an editing block are for.
function heading(text) {
  const hash = /^(#{1,3}) /.exec(text);
  return hash ? ' h' + hash[1].length : '';
}

// startEditing opens a block. caret is where to stand in it, the end of the
// text when it is left out, which is what a click asks for and what the arrows
// and the joins name for themselves.
function startEditing(id, caret) {
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
  // A block this page has not drawn yet has nothing to replace: it is a block
  // the server made a moment ago, and the render its own event causes puts this
  // node where the row is. The caret and the height survive that, because
  // afterRender carries both.
  const at = Math.min(caret ?? w.text.length, w.text.length);
  editing.area.focus();
  editing.area.setSelectionRange(at, at);
}

// editor builds the block as a textarea and makes it the one this tab is in.
// It holds nothing: the text lives in the entry, and this is a way of typing
// into it.
//
// id is nought for the provisional editor a split draws, which stands for a
// block the server has not made yet. It has no block id on the node and no
// entry behind it, and the keys below that would name a block are off in it
// until the ack gives it one.
function editor(id, text) {
  const area = el('textarea', { spellcheck: 'false', 'aria-label': 'This block as markdown' });
  area.value = text;
  // The mirror is this text again, under the textarea in the same box, so that
  // somebody else's caret can be drawn in the middle of it: a textarea holds
  // nothing but text. Its own text is transparent and the textarea over it has
  // no background of its own, so what shows through is their caret and nothing
  // else. The two share one grid cell, which is what keeps them the same size
  // as each other whatever fit makes of the height.
  const mirror = el('div', { class: 'mirror', 'aria-hidden': 'true' });
  const me = user(state.me);
  const node = el('div', { class: 'blk editing' + heading(text), 'data-b': id || null });
  node.append(el('div', { class: 'tbox' }, mirror, area),
    el('span', { class: 'who me ' + me.colour, text: 'you' }));

  // The mirror is drawn here rather than at each of the places the text
  // changes, because fit is already called at every one of them: a height that
  // is out of step with the text and a caret that is are the same mistake.
  const fit = () => {
    area.style.height = 'auto';
    area.style.height = area.scrollHeight + 'px';
    reflect(ed);
  };
  // The listeners read the block off this rather than closing over the argument
  // above, because a provisional editor becomes the editor of a real block the
  // moment its insert is acked and all of them have to follow it there.
  const ed = { id, node, area, mirror, fit };
  area.addEventListener('input', () => { typed(ed.id); fit(); });
  area.addEventListener('keydown', (e) => key(e, ed));
  area.addEventListener('paste', (e) => pasted(e, ed));
  // A blur raised by the rebuild is not somebody leaving the block. The node is
  // put back by the same render and afterRender takes the caret with it.
  area.addEventListener('blur', () => { if (!rendering) leave(ed.id); });
  editing = ed;
  where(id ? 'block:' + id : docWhere());
  // The height is set after the node is in the page, because a detached
  // textarea has no scroll height to measure.
  queueMicrotask(fit);
  notice(id);
  return node;
}

// reflect keeps the mirror in step with what is in the textarea and with where
// the others are standing in it. It draws that one node rather than the page,
// because the editor is carried across every render and a rebuild would take
// the caret out of it. Everything that changes either of those calls fit, and
// fit calls this.
//
// Their offsets are counted in the text the server holds at the entry's base,
// which is what the entry last sent. What has been typed since is one span of
// that text, so each offset is carried through the span the way a merge
// carries this person's own caret. A caret stated against any other version
// cannot be placed at all and keeps the last place it was drawn.
function reflect(ed = editing) {
  if (!ed) return;
  const w = ed.id ? work.get(ed.id) : null;
  const text = ed.area.value;
  const here = w ? carets(ed.id, inside(ed.id), w.base, (n) => carry(w.sent, text, n)) : [];
  clear(ed.mirror);
  add(ed.mirror, raw(text, here));
}

// caretsMoved is the light redraw, for a presence frame that says nothing but
// that somebody's caret has moved inside the block they were already in. A
// render remakes the rail, the top bar and every card in the board with fresh
// listeners, and clearing the work area takes with it whatever a reader had
// selected on the page; at five frames a second per person moving a caret that
// is not a thing to do. So this touches the open document's blocks and nothing
// else: each is asked for again, the drawn cache hands back the same node for
// every block nothing changed in, and only a block that came back as a
// different node is put on the page.
function caretsMoved() {
  // Mid rebuild the page is not the page yet, and the render that is running
  // draws every one of these blocks itself on the way past.
  if (rendering) return;
  const doc = current();
  for (const b of doc ? doc.blocks || [] : []) {
    // A block waiting on an answer is built fresh every time it is asked for
    // and draws no caret at all, so it is left alone: replacing it would throw
    // away the height of its textarea for nothing.
    const w = work.get(b.id);
    if (w && w.status !== 'ok') continue;
    const was = $(`#doc .blk[data-b="${b.id}"]`);
    if (!was) continue;
    // A block somebody has tabbed to keeps its node: taking it out of the page
    // would drop the focus to the body, and every fifth of a second at that.
    // It catches up on the next render like everything else.
    if (was.contains(document.activeElement)) continue;
    const node = blockNode(b);
    if (node !== was) was.replaceWith(node);
  }
  // The block this tab is in comes back out of the loop above as the editor's
  // own node, which is already where it belongs. Somebody else's caret in that
  // one is drawn in the mirror instead.
  reflect();
}

// The socket is told about it here rather than importing it there, because
// this module already imports that one.
onCarets(caretsMoved);

// tellCaret says where this tab is standing, which is worth saying only when
// the offsets mean something to the others: a real block, nothing unsaved, and
// nothing of this tab's waiting in the outbox, so the text they will count in
// is the text the server holds at the version named. While something is
// unsaved the next save is at most two seconds away and this says it then;
// until it does, the marker says who is in the block and their last caret
// stands where it was.
function tellCaret() {
  if (!editing || !editing.id || !online()) return;
  const w = work.get(editing.id);
  if (!w || w.status !== 'ok' || w.text !== w.sent || state.waitingHere) return;
  where(formatWhere(editing.id, w.base, editing.area.selectionStart, editing.area.selectionEnd));
}

// caretRate is how often a moving caret is worth a frame, and moveCaret is the
// one way a caret is ever sent: everything that might have moved it, or made
// it mean something again, arms this timer and the timer does the sending.
// Sending from where it happened would be a frame per happening, and one of
// those happenings is the echo of this tab's own `where` coming back as a
// presence frame and a render: a held arrow key would then be forty frames a
// second rather than five, and the allowance is what a save depends on.
//
// The timer holds nothing of its own: whatever it finds when it fires is what
// goes, so there is nothing in it to be stale when the block, the editor or
// this person's place has changed in the meantime. Sending the same string
// twice is net.js's to drop.
const caretRate = 200;
let caretTimer = 0;

function moveCaret() {
  if (caretTimer) return;
  caretTimer = setTimeout(() => { caretTimer = 0; tellCaret(); }, caretRate);
}

// One listener for every editor there will ever be, because selectionchange is
// the only event that fires for all the ways a caret moves and the editor is
// given up and made again all the time.
document.addEventListener('selectionchange', () => {
  if (editing && document.activeElement === editing.area) moveCaret();
});

// The keys that do something to the block rather than to the text in it. Each
// one falls through to what a textarea does on its own the moment its
// conditions are not met, so nothing here takes a key away from somebody
// without giving them what it promised instead.
function key(e, ed) {
  // Escape leaves the block. It stops here so that it does not also close the
  // drawer or the palette on its way up.
  if (e.key === 'Escape') { e.stopPropagation(); ed.area.blur(); return; }
  // A provisional editor has no block to name, and a key pressed mid
  // composition belongs to the input method.
  if (!ed.id || e.isComposing) return;
  if (e.key === 'Enter' && !e.shiftKey) { pressedEnter(e, ed); return; }
  if (e.key === 'Backspace') { pressedBackspace(e, ed); return; }
  if (e.key === 'ArrowUp' || e.key === 'ArrowDown') pressedArrow(e, ed);
}

// Enter writes the next item of a list into this block, or cuts the block in
// two at the caret. The next item is text in a block that already exists, so it
// is typing and works with no connection like any other typing. Cutting one in
// two is not: it needs the server to make the block below, and it is a plain
// newline without one, while a split is already in the air, or in a block
// waiting on an answer, because a block cut in two under a refusal would leave
// the half below with nothing to go back into when the refusal is discarded.
// Shift+Enter is always a plain newline.
//
// So is Enter inside a fenced code block, or over a selection that reaches into
// one: a split there would leave a fence open in the block above and a block
// below starting inside one, and the server keeps a fence in one block whatever
// is in it. This decides not to split; it does not decide where a block ends,
// which stays the server's alone.
function pressedEnter(e, ed) {
  if (inFence(ed.area.value, ed.area.selectionStart, ed.area.selectionEnd)) return;
  const what = enter(ed.area.value, ed.area.selectionStart, ed.area.selectionEnd);
  if (what.kind === 'list') {
    e.preventDefault();
    ed.area.value = what.text;
    ed.area.setSelectionRange(what.caret, what.caret);
    typed(ed.id);
    ed.fit();
    return;
  }
  const w = work.get(ed.id);
  if (!online() || pending || (w && w.status !== 'ok')) return;
  e.preventDefault();
  split(ed, what.before, what.after);
}

// split is Enter in the middle of a block: this one keeps what was in front of
// the caret, and what was behind it becomes the block below. That block does
// not exist yet, so an editor holding the text is drawn where it will be and
// the caret goes straight into it. Nothing typed during the round trip is lost
// or lands in the wrong block: the ack takes the text out of that editor as it
// stands then, wherever the person has got to.
function split(ed, before, after) {
  const id = ed.id;
  const doc = current();
  ed.area.value = before;
  ed.fit();
  typed(id);
  // The half that stays goes up the ordinary way. leave sends what the entry
  // holds and gives the editor up, which is what has to happen anyway now the
  // caret is moving out of it.
  leave(id);

  const node = editor(0, after);
  pending = { anchor: id, ed: editing };
  const was = $(`#doc .blk[data-b="${id}"]`);
  if (was) was.after(node);
  editing.area.focus();
  editing.area.setSelectionRange(0, 0);
  insert(doc.id, { after: id, text: after, whole: true })
    .then(inserted)
    // Offline here is the network going between the check above and the send,
    // or fifteen seconds of trying and never being answered. The text goes back
    // into the block it came from either way; saying so twice, once on the bar
    // and once as a line of its own, would be saying it about a block nobody
    // can see.
    //
    // A socket that dies with the insert in the air is no longer one of these:
    // the frame carries a key and goes again on the next socket, and a server
    // that already applied it says so rather than making a second block.
    //
    // ponytail: what is left is the frame that was in the air when the fifteen
    // seconds ran out. The server may apply it a moment later, and by then this
    // has put the paragraph back on the end of the block it was split from, so
    // the block arrives as an ordinary event and that paragraph is on the page
    // twice. Nothing is lost and both are visible. Closing it needs the editor
    // to be able to draw the insert before the answer, which is the change that
    // makes Enter work with no connection at all.
    .catch((err) => unsplit(err instanceof Offline ? '' : err.message));
}

// inserted is the split's ack. The row exists holding exactly what was sent, so
// the entry starts from it and takes whatever else has been typed into the
// provisional editor since. If the person is still in that editor it becomes
// the ordinary editor of the block; if they have moved on, the entry still has
// their text and sends it.
function inserted(ev) {
  if (!pending) return;
  const ed = pending.ed;
  const id = ev.after.id;
  const w = {
    text: ed.area.value, base: ev.after.version, sent: ev.after.text,
    first: 0, timer: 0, flight: false, status: 'ok',
  };
  work.set(id, w);
  const mine = editing === ed;
  pending = null;
  ed.id = id;
  ed.node.dataset.b = id;
  if (mine) {
    where('block:' + id);
    notice(id);
    arm(id, w);
  } else {
    ed.node.remove();
    save(id);
  }
  emit();
}

// unsplit is the split coming back refused, or with no socket for it to have
// gone down at all. The block was cut in two on the page and only one half is
// on the server, so the halves go back together: what the provisional editor
// holds goes on the end of the block it was split from and the caret goes back
// to the join. If that block has gone as well there is nowhere to put the
// words and the line below says so, which is the one case here that loses
// anything: a block deleted by somebody else while the server was refusing the
// half that came out of it.
function unsplit(why) {
  if (!pending) return;
  const { anchor, ed } = pending;
  const text = ed.area.value;
  pending = null;
  // The editor is given up and its node taken out before the block below is
  // opened, so the blur that raises is still a provisional editor leaving and
  // still means nothing.
  if (editing === ed) editing = null;
  ed.node.remove();
  if (why) say(why);
  const join = putBack(anchor, '\n' + text);
  if (join >= 0) startEditing(anchor, join);
}

// putBack is text this editor drew that the server would not take. It goes on
// the end of the block it came from, through that block's entry and the
// ordinary save, so the words are on the page and on their way up rather than
// nowhere. It answers where the join is, or minus one when the block it came
// from has gone as well.
//
// The entry is read rather than the text the split was made from: the block may
// have been merged or saved and let go since, and what is in the entry now is
// where those words actually are.
function putBack(id, text) {
  const w = entryOf(id);
  if (!w) {
    say('The block that text came out of is gone, and what was under it went with it.');
    return -1;
  }
  const join = w.text.length;
  w.text = w.text + text;
  if (openOn(id)) {
    editing.area.value = w.text;
    editing.area.setSelectionRange(join, join);
    editing.fit();
  }
  arm(id, w);
  return join;
}

// Backspace at the start of a block joins it to the one above, which is how an
// empty block goes and how two paragraphs become one. The text goes on the end
// of the block above and this one is deleted; a refusal leaves the text in both
// of them, which is visible and loses nothing.
function pressedBackspace(e, ed) {
  const { area } = ed;
  if (area.selectionStart !== 0 || area.selectionEnd !== 0 || !online()) return;
  const w = work.get(ed.id);
  if (w && w.status !== 'ok') return;
  const list = neighbours();
  const at = list.findIndex((b) => b.id === ed.id);
  if (at <= 0) return;
  const above = work.get(list[at - 1].id);
  if (above && above.status !== 'ok') return;
  e.preventDefault();
  const prev = list[at - 1].id;
  const join = putBack(prev, area.value);
  if (join < 0) return;
  // This block's entry goes with the block. A save of it already in the air
  // comes back to no entry, which acked takes as nothing left to do.
  if (w) { clearTimeout(w.timer); work.delete(ed.id); }
  send('block.delete', { block: ed.id }).catch((err) => say(err.message));
  startEditing(prev, join);
  save(prev);
}

// The arrows walk out of a block the way they walk out of a line: up at the
// start opens the one above at its end, down at the end opens the one below at
// nought. With Alt they move the block itself, and the editor stays open and
// rides the render the ack causes.
function pressedArrow(e, ed) {
  const up = e.key === 'ArrowUp';
  const { area } = ed;
  const list = neighbours();
  const at = list.findIndex((b) => b.id === ed.id);
  if (at < 0) return;
  if (e.altKey) {
    if (up ? at === 0 : at === list.length - 1) return;
    e.preventDefault();
    const after = up ? (at > 1 ? list[at - 2].id : 0) : list[at + 1].id;
    send('block.move', { block: ed.id, after }).catch((err) => say(err.message));
    return;
  }
  if (area.selectionStart !== area.selectionEnd) return;
  if (up ? area.selectionStart !== 0 : area.selectionEnd !== area.value.length) return;
  const next = list[up ? at - 1 : at + 1];
  if (!next) return;
  e.preventDefault();
  startEditing(next.id, up ? null : 0);
}

// neighbours is the open document's blocks in the order they are drawn, which
// is what above and below mean to every key here.
function neighbours() {
  const doc = current();
  return doc ? doc.blocks || [] : [];
}

// A paste of more than one paragraph becomes more than one block, because a
// block is a paragraph. The first goes in at the caret, where the caret stays,
// and everything after it goes up as one ordinary insert with what followed the
// caret carried on the end: the server cuts that into blocks by the same rule
// it cuts every other paste by, in one transaction, and the whole paste lands
// in one frame. Sent a block at a time it would instead arrive a round trip at
// a time, in an order a refusal halfway could break, and a long one would run
// into the limit on how fast a tab may send.
//
// A refusal puts the whole of that remainder back on the end of this block,
// through the ordinary entry, so nothing pasted is lost.
function pasted(e, ed) {
  if (!ed.id || !online() || !e.clipboardData) return;
  const w = work.get(ed.id);
  if (w && w.status !== 'ok') return;
  const parts = chunks(e.clipboardData.getData('text'));
  if (parts.length < 2) return;
  e.preventDefault();
  const { area } = ed;
  const rest = parts.slice(1).join('\n\n') + area.value.slice(area.selectionEnd);
  area.value = area.value.slice(0, area.selectionStart) + parts[0];
  area.setSelectionRange(area.value.length, area.value.length);
  typed(ed.id);
  ed.fit();
  const id = ed.id;
  insert(current().id, { after: id, text: rest }).catch((err) => {
    say(err.message);
    putBack(id, '\n\n' + rest);
  });
}

// The + after the last block is how a document grows a paragraph without being
// in one already, and how an empty document is started. It is quiet until the
// pointer or the focus is on it, like the other affordances.
function addBlock(doc) {
  const off = !online();
  const last = (doc.blocks || []).at(-1);
  return el('button', {
    type: 'button', class: 'addblk', id: 'blocknew', text: '+', 'aria-label': 'Add a block',
    disabled: off,
    title: off ? 'A new block needs the server, and this device has no connection.' : 'Add a block',
    onclick: () => insert(doc.id, { after: last ? last.id : 0, text: '', whole: true })
      .then((ev) => startEditing(ev.after.id))
      .catch((err) => say(err.message)),
  });
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
// give the text up, the pruning of a block that is gone, and the join above,
// which deletes the block the entry was about.
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
  // The entry is clean at a new version, which is the moment an offset in it
  // means something to anybody again. This writes nothing down: it asks for
  // the caret to be sent, and nothing else.
  moveCaret();
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
//
// What cannot go is what was typed into a provisional editor inside the round
// trip of its split: until the insert is acked there is no block to address a
// save to. What the server has of it is the paragraph as it stood when Enter
// was pressed, which is what the insert carried. That round trip is now as long
// as the insert keeps trying, up to the fifteen seconds live allows it, rather
// than ending the moment a socket goes; a tab closed inside a reconnection
// therefore loses more of what was typed into it than it used to.
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
    el('p', { text: 'Changes are saved as you type. A version is kept when you ask, every two minutes while somebody is editing, and before markdown for the whole document is read back in from the file or from the source view.' }),
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
  // One reason covers both roads markdown for a whole document comes back in
  // by: the file on disk, and Save in the source view.
  if (reason === 'pre-import') return 'Before markdown was read in';
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
