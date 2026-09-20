// The document under the board: the tabs, the blocks, the source view, the
// history with its diff, and the one choice a conflict asks for.
//
// A block is markdown. Clicking one shows its source in a textarea sized to fit
// and a heading keeps the size it renders at; what is typed goes up every few
// hundred milliseconds as a block.set the server stores exactly as it was sent,
// with the version the editor started from, so somebody else's change in
// between is merged on the server or comes back as a choice.

import { $, el, add, clear, inline, say, editable, ask } from './dom.js';
import { state, user, byHandle, emit, hold, canEdit, makeLocal, writeLocal, unmakeLocal, rekeyLocal, onSettled, order, target, localOf } from './state.js';
import { send, newKey, where, onCarets, caughtUp, count, chosen, Conflict, Offline } from './net.js';
import { replace } from './api.js';
import { retext, unqueue, file } from './offline.js';
import { rebase, enter, chunks, carry, span, inFence, parseWhere, formatWhere } from './blocktext.js';
import { parts } from './blockparts.js';
import * as undo from './undo.js';
import { movable, carrying } from './drag.js';

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
//   rename   on a refused block whose insert was answered and gone, so that
//            trying again is a command under a name of its own
//   filed    the outbox row the unanswered question is kept in, zero for a
//            block with nothing to answer or a browser with no storage
//
// The entry itself lives in this tab and dies with it. What must not is the
// question in it: keepClash files a conflict or a refusal in the outbox as a
// refused row, which is the row a refusal during a drain already makes, and
// unanswered() brings it back to this block after a reload and takes it off
// this block when it is answered somewhere else.
const work = new Map();

// newBlock draws a block the server has not made yet and sends the command that
// makes it. The block is a row in the document like any other, with a negative
// id, so it renders, takes an editor and takes typing while it is still this
// tab's own; the ack puts the real block in its place underneath whoever is
// standing in it.
//
// The command goes the ordinary way, which is what makes Enter, the + and a
// paste work with no connection at all: up now if there is a socket, into the
// outbox if there is not, replayed under the same key either way.
function newBlock(document, after, text, whole) {
  // after is where it goes on the page, which is a block this tab made as
  // readily as one the server has. to is the same place said in the one way the
  // server can hear it, worked out once here and sent as it stands, so that a
  // block made under one the server later gives an id to still names the
  // command it was made under rather than an id that meant nothing then.
  const row = makeLocal({
    document_id: document, text, whole, key: newKey(),
    after, to: anchorOf(blockAnywhere(after)),
  });
  insert(row);
  return row;
}

// anchorOf says where a new block goes in the one way the server can hear it: a
// block the server has by its id, and one this tab has drawn by the key of the
// command that makes it, since a negative id means nothing anywhere but here.
// Nothing at all is the head of the document.
//
// Everything in the app that puts an insert on the wire says where it goes
// through this, which is what keeps a block drawn here from being named to the
// server by an id it does not have.
export function anchorOf(block) {
  if (!block) return { after: 0 };
  return block.id < 0 ? { after_key: block.key } : { after: block.id };
}

// insert sends the command that makes a local block, and is also how one the
// server would not take goes again. Where it goes was worked out when the block
// was drawn, by anchorOf above, and is sent as it stands.
//
// again marks the second try, which is the one case worth one: the socket went
// between the check and the frame, and the same command goes into the outbox
// under the same key. A browser with no storage to keep it in cannot do even
// that, and there this is a refusal like any other.
// flying is the key of every insert this tab has sent and has no answer to. A
// command that is in no store is either one of these or one that has been
// answered and left, and those two want opposite things from a save, so this is
// what tells them apart.
const flying = new Set();

function insert(row, again) {
  const w = work.get(row.id);
  // The row is read again rather than trusted: a save of a block this tab made
  // writes the text into the row as well as into the command, so the one this
  // was called with may be a copy from before that.
  const text = w ? w.text : (blockAnywhere(row.id) || row).text;
  flying.add(row.key);
  send('block.insert', insertArgs(row, text), state.open, { key: row.key })
    // An answer that came back is the block itself arriving, and applying it is
    // what takes the row this tab drew off the page: the key on the event is
    // what the two are matched by, and state.js does it wherever the event
    // comes from. Null is the command going into the outbox instead, which is
    // this entry agreeing that the text has reached the only place it can.
    .then((ev) => { flying.delete(row.key); if (!ev) acked(row.id, text, 0, null); })
    .catch((err) => {
      flying.delete(row.key);
      if (err instanceof Offline && !again) { insert(row, true); return; }
      refusedInsert(row, err);
    });
  status();
  emit();
}

// insertArgs is the command that makes a block this tab drew, with where it
// goes said as anchorOf worked it out when the block was drawn. It is asked for
// twice: once by the command itself and once by the row that keeps that command
// when the server would not take it, and the two have to be the same command,
// because drawQueued draws the block again from those arguments after a reload.
const insertArgs = (row, text) => ({ document: row.document_id, text, whole: row.whole, ...row.to });

// bound is the real block arriving for the one this tab drew. That row has just
// gone, in the same tick the real one was applied, so the paragraph is never
// drawn twice and never blinks out; what is left for this module is the text
// somebody has typed into it and the editor they are standing in. The text goes
// up as an ordinary save if the server has not got it, and the editor is
// re-keyed in place rather than rebuilt, so it keeps its caret, its selection
// and its height.
function bound(row, now) {
  const w = work.get(row.id);
  if (!row.whole) {
    // A paste is one insert the server cuts into blocks, so this is the first
    // of several and the row this tab drew stood for all of them: there is no
    // one block for the entry or an editor to move to, and the editor goes with
    // the row through afterRender. This is the one place an entry is dropped
    // rather than moved or answered, because the block it was about is not one
    // block any more.
    //
    // What somebody typed into it after the command had gone is the one thing
    // here the server's cut cannot hold, so those words go on the end of the
    // first block the paste made, through the ordinary entry. The first,
    // because this event names it and the rest arrive as their own events with
    // nothing to say which is the last of them. What goes back is the one run
    // of text that differs, which is what a person typing makes; an edit in one
    // place and another somewhere else in the same block, between the command
    // going and this answer, leaves only the first of them.
    const lost = w && w.text !== w.sent ? span(w.sent, w.text).ins : '';
    // The entry goes, so the question it was keeping goes with it. Left filed,
    // the row would be a refusal about a block that arrived after all, drawn
    // again out of the outbox by the next reload as a paragraph nobody wrote.
    if (w) { clearTimeout(w.timer); forgetClash(w); work.delete(row.id); }
    if (lost.trim()) putBack(now.id, '\n\n' + lost);
    return;
  }
  if (w) {
    work.delete(row.id);
    w.base = now.version;
    w.sent = now.text;
    w.flight = false;
    // A refusal that said no block was ever coming for this one was wrong: here
    // it is. The reason goes with it, and what was typed under it goes up as an
    // ordinary save of the real block, which is what arm below sends. This runs
    // before notice, which draws the refusal from the entry it has just moved.
    // The row that was keeping the question outlives this tab, so it goes too,
    // or a reload would draw the question again over a block that has an id.
    forgetClash(w);
    w.status = 'ok';
    delete w.reason;
    delete w.rename;
    work.set(now.id, w);
  }
  // What Ctrl+Z takes back in this paragraph was typed before the server had
  // given it an id, and it is the same paragraph's to take back now that it
  // has one. It has to move before the next render, whose keep drops the
  // histories of blocks that are no longer on the page.
  undo.move(row.id, now.id);
  if (editing && editing.id === row.id) {
    editing.id = now.id;
    editing.node.dataset.b = now.id;
    where('block:' + now.id);
    notice(now.id);
  }
  // A block made under this one still points at the id it is losing, and sits
  // on a key guessed under the one this row was guessed at. The server has just
  // put this block above that guess, so the one below would be drawn above it
  // until its own insert landed: both are moved on, the id so that a refusal
  // knows which block to put the words back into, and the key so that the
  // paragraphs stay in the order they were written in.
  for (const d of documents()) {
    let moved = false;
    for (const b of d.blocks || []) {
      if (b.after !== row.id) continue;
      b.after = now.id;
      b.position = now.position + '0';
      moved = true;
    }
    if (moved) (d.blocks || []).sort(order);
  }
  if (w) arm(now.id, w);
}

onSettled(bound);

// refusedInsert is the server saying no to a block this tab has already drawn.
// The words go on the end of the block it was made under, through that block's
// entry and the ordinary save, and the block itself comes off the page. If
// there is nowhere to put them, because it was made under nothing or what it
// was made under has gone, the block stays where it is with the reason above it
// and the two answers beside it: what somebody typed is not something to drop
// quietly.
function refusedInsert(row, err) {
  const w = work.get(row.id);
  const text = w ? w.text : (blockAnywhere(row.id) || row).text;
  const why = err instanceof Offline ? 'This device has nowhere to keep that block.' : err.message;
  // Where it was made, which is a block the server has or one this tab made.
  // Either can take the words back; a block made under nothing, which is the +
  // in an empty document, is the case that cannot.
  const above = row.after ? blockAnywhere(row.after) : null;
  if (above) {
    if (w) { clearTimeout(w.timer); forgetClash(w); work.delete(row.id); }
    if (editing && editing.id === row.id) editing = null;
    unmakeLocal(row.key);
    say(why);
    // A paste went in under the block it was pasted into and a split under the
    // block it was cut out of, which is one blank line and one newline: the
    // same two joins the two of them make when they are taken back.
    const join = putBack(above.id, (row.whole ? '\n' : '\n\n') + text);
    if (join >= 0) startEditing(above.id, join);
    return;
  }
  refusedHere(row, why, text);
}

// refusedHere leaves the words where they were written, with the reason above
// them and the two answers a refusal has. It is where a refusal comes to when
// there is nowhere to put the words back: the + in an empty document, and a
// command that has been answered and gone without this tab hearing which block
// it made.
//
// ponytail: a refusal held in this tab goes with a reload, as every refusal
// held here does. The branch that files unanswered conflicts and refusals in
// the outbox is where that is answered, for these as for the rest.
function refusedHere(row, why, text) {
  const kept = entryOf(row.id);
  if (!kept) return;
  kept.text = text;
  kept.status = 'refused';
  kept.reason = why;
  clearTimeout(kept.timer);
  kept.timer = 0;
  kept.first = 0;
  say(why);
  keepClash(row.id);
  notice(row.id);
  emit();
  status();
}

// named says whether a block that is still this tab's own is what another one
// is waiting to be made under. That insert carries this block's key and nothing
// else, so taking this one back would leave it with nowhere to go.
//
// A row renamed for a retry answers to the name it was drawn under as well: the
// insert underneath it was filed against that one, and that name still says on
// the server which block it belongs under.
function named(id) {
  const row = blockAnywhere(id);
  if (!row || !row.key) return false;
  return documents().some((d) => (d.blocks || []).some((b) => b.to && b.to.after_key
    && (b.to.after_key === row.key || b.to.after_key === row.former)));
}

// Online is a socket and a network. Joining two blocks and deleting one still
// ask for both: the delete is not drawn until the server answers, so a join
// made with nothing to answer it would leave the paragraph in both blocks.
// Making a block asks for neither any more.
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

// gone is whether a block of the open proposition has been deleted out from
// under something that is still holding words for it, which is the one thing
// the activity panel cannot work out from a refused row on its own.
export const gone = (id) => !blockAnywhere(id);

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
    }
    for (const id of drawn.keys()) if (!ids.has(id)) drawn.delete(id);
    // seen goes the same way, by its own keys rather than drawn's: the block
    // this tab is typing in is never in drawn, and its mirror writes into seen
    // like every other block. This is where a deleted block and a document
    // switched out from under the page both drop what was drawn in them.
    for (const id of seen.keys()) if (!ids.has(id)) seen.delete(id);
    // And what Ctrl+Z would take back in each of them, by the same keys. A
    // block that is gone takes its steps with it, and so does every block of a
    // document the tab has switched away from: that tab can be clicked back,
    // and the paragraph then starts again from what it says, which is the same
    // answer a block whose text moved underneath gets in editor below.
    undo.keep(ids);
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
  unanswered();
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
//
// A block waiting on an answer keeps its row in the outbox when it is pruned.
// The block is gone and the question cannot be asked on it any more, but the
// words are still the person's: the panel says the block has been deleted and
// let it go is what drops them.
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

// making is a block this tab has drawn that the server has not made yet. It is
// work outstanding on its document whatever its entry says, because the insert
// still has to land, and the entry of a block nobody is typing in is dropped as
// soon as its text has reached the outbox.
const making = (doc) => (doc ? doc.blocks || [] : []).some((b) => b.id < 0);

// A block on its way into a document is outstanding on that document too, so
// its tab carries the dot until it is made.
const held = (doc) => making(doc) || entries(doc).some(busy);

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
  // whether or not it has an entry to be counted in. A block on its way into
  // another document is that document's line to say, not this one's. With no
  // socket it is not being saved and saying so would be a story: it is waiting
  // for one, which is what the bar at the foot of the page says as well.
  if (making(doc)) return state.connected ? 'Saving…' : 'Waiting to save';
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
  src.key = src.key || newKey();
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
  //
  // A paragraph that merged is told about here and nowhere else, because this
  // is the only save that leaves the markdown open: what is in front of them
  // is now behind the document for those paragraphs, and pressing Save again
  // would write it back over the words that came in. A save that merged and
  // conflicted with nothing closes the view, and there is nothing to warn
  // about.
  const one = left === 1;
  const merged = (answer.merged || []).length;
  const take = 'To take what they wrote instead, read the markdown again, which throws away what is in front of you.';
  const lead = merged
    ? `Everything else you wrote went in, and ${merged} other ${merged === 1 ? 'paragraph' : 'paragraphs'} took in words somebody else wrote. Saving again writes your wording over theirs, in ${merged === 1 ? 'that one' : 'those'} as well. ${take}`
    : `Everything else you wrote went in. Saving again writes ${one ? 'your paragraph' : 'your paragraphs'} over theirs. ${take}`;
  const yes = await ask(
    `${left} ${one ? 'paragraph was' : 'paragraphs were'} left as ${one ? 'it is' : 'they are'}, because somebody else changed ${one ? 'it' : 'them'} while you were writing.`,
    lead, 'Read it again');
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
    node.addEventListener('click', (e) => {
      // The pointer that has just carried the block ends in a click as well,
      // and that one finishes the drag rather than asking to write in it. So
      // does a press on the handle that moved too little to carry anything:
      // the handle says grab and is not where anybody asks to write.
      if (!carrying() && !onLink(e)) startEditing(b.id);
    });
    node.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !onLink(e)) { e.preventDefault(); startEditing(b.id); }
    });
    grip(node, b.id);
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

// grip is the handle a block is carried by, in the margin beside it, and the
// drag it starts. Only the handle carries the block: pressing the text of one
// has to go on meaning what it already means, which is click to write in it,
// drag across it to select what it says, and follow a link in it. It is not a
// button and takes no focus, because the keyboard already moves a block with
// Alt and an arrow, which is what the title says; this is the pointer's way,
// and the only one a phone has.
function grip(node, id) {
  // Only a block the server has can be named in a command, so only one with a
  // block id of its own is given a handle at all, rather than one that would
  // ask for something nobody could answer.
  if (id <= 0) return;
  node.append(el('span', {
    class: 'grip', 'aria-hidden': 'true', text: '≡',
    title: 'Drag to move this block. Alt with an arrow key moves it too.',
  }));
  movable(node, {
    zone: '#docwrap', list: (z) => $('#doc', z), rows: '.blk', handle: '.grip',
    drop: () => {
      const list = neighbours();
      const at = list.findIndex((b) => b.id === id);
      // Somebody deleted the block while it was in the air. There is nothing
      // left to move and nothing to move it among.
      if (at < 0) return;
      // Where it was, by the same rule the page is read by below: the nearest
      // block above it the server has. It is read out of the state rather than
      // off the page, because a move by somebody else arriving mid drag is held
      // off the page until the drop and is still where this one started.
      let was = 0;
      for (let i = at - 1; i >= 0 && !was; i--) if (list[i].id > 0) was = list[i].id;
      const after = previousBlock(node);
      // A block let go where it already was asks the server for nothing.
      if (after === was) return;
      send('block.move', { block: id, after }).catch((err) => { say(err.message); emit(); });
    },
  });
}

// previousBlock is what a drop landed behind: the nearest row above it on the
// page carrying the id of a block the server has, or nought for the head of the
// document. A row without one is not somewhere a block can be put after, and
// the list holds two of those: the button that adds a block, and the editor
// drawn where a split is going before the block it stands for exists.
function previousBlock(node) {
  for (let at = node.previousElementSibling; at; at = at.previousElementSibling) {
    const id = Number(at.dataset.b);
    if (id > 0) return id;
  }
  return 0;
}

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
  else putNear(id, node);
  const at = Math.min(caret ?? w.text.length, w.text.length);
  editing.area.focus();
  editing.area.setSelectionRange(at, at);
}

// putNear puts an editor for a block the page has not drawn yet where the row
// belongs, rather than leaving it for the render the row causes. A block made a
// moment ago is one of these, and so is every block this tab draws before the
// server has made it. A detached textarea cannot take the keyboard: focus on
// one does nothing, the block the person was in keeps it, and the next key they
// press goes to that editor while this one is the editor the page thinks it is
// in. The word lands in the wrong entry and is saved there.
//
// Where it belongs is after the nearest block above it that is drawn, or before
// the nearest below when there is none, or at the end of the document, which
// is in front of the + that adds one.
function putNear(id, node) {
  const list = neighbours();
  const at = list.findIndex((b) => b.id === id);
  for (let i = at - 1; i >= 0; i--) {
    const above = $(`#doc .blk[data-b="${list[i].id}"]`);
    if (above) { above.after(node); return; }
  }
  for (let i = at + 1; i < list.length; i++) {
    const below = $(`#doc .blk[data-b="${list[i].id}"]`);
    if (below) { below.before(node); return; }
  }
  const doc = $('#doc');
  if (doc) doc.insertBefore(node, $('#blocknew'));
}

// editor builds the block as a textarea and makes it the one this tab is in.
// It holds nothing: the text lives in the entry, and this is a way of typing
// into it.
//
// id is negative for a block the server has not made yet, which is an ordinary
// block here and an ordinary entry behind it. The two keys that would name it
// to the server, the join and the move, are the ones that say no below.
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
  // above, because the editor of a block this tab made becomes the editor of the
  // real block the moment its insert is acked, and all of them follow it there.
  const ed = { id, node, area, mirror, fit, was: null };
  // What Ctrl+Z in this block will take back, kept while the document is open,
  // so leaving a paragraph and coming back finds what was written in it still
  // there to take back.
  //
  // The history is only that paragraph's if it still says what the paragraph
  // says. Between the editor closing and it opening again there is usually no
  // entry to answer for the block, because finished drops one the moment the
  // block is clean, so somebody else's save, a join from the block below, or
  // an import can move the text with nothing here to notice it. Undoing back
  // into what this person wrote would then be sent from the version the block
  // has now and stored over them without a word. So a block that reads
  // something else starts again from what it reads, and only one that reads
  // what this tab last left keeps its steps. The caret is put at the end,
  // which is where a click that names nowhere lands.
  const open = { text, start: text.length, end: text.length };
  const history = undo.of(id, open);
  if (history.now() === text) history.record(open);
  else undo.reset(id, text);
  area.addEventListener('input', (e) => { stepped(ed, e); typed(ed); fit(); });
  area.addEventListener('beforeinput', (e) => {
    // The Edit menu, a phone shaken, three fingers swiped: the same two things
    // the keys below do, arriving as an input type rather than as a key.
    if (e.inputType === 'historyUndo' || e.inputType === 'historyRedo') {
      e.preventDefault();
      stepBack(ed, e.inputType === 'historyRedo');
      return;
    }
    // Where the selection was before this change, which is how a run of typing
    // ends when somebody clicks somewhere else in the block and writes there.
    ed.was = { start: area.selectionStart, end: area.selectionEnd };
  });
  // Half a composed word is not a place to undo back to, so nothing is recorded
  // while an input method is in the middle of one. The finished word is.
  area.addEventListener('compositionend', () => stepped(ed, null));
  area.addEventListener('keydown', (e) => key(e, ed));
  area.addEventListener('paste', (e) => pasted(e, ed));
  // A blur raised by the rebuild is not somebody leaving the block. The node is
  // put back by the same render and afterRender takes the caret with it.
  area.addEventListener('blur', () => { if (!rendering) leave(ed.id); });
  editing = ed;
  // Nobody else can be told about a block the server does not have: a negative
  // id means nothing outside this tab, and an offset in a block no other tab
  // can see means nothing to anybody. Being in the document is what they hear.
  where(id > 0 ? 'block:' + id : docWhere());
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
  const w = work.get(ed.id);
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
    // A block in the hand keeps its node for a harder reason: replacing it
    // would leave the pointer carrying a node that is no longer on the page,
    // and the page holding two nodes for one block until the drop. Both catch
    // up on the next render like everything else.
    if (was.contains(document.activeElement) || was.classList.contains('dragging')) continue;
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
  if (!editing || editing.id <= 0 || !online()) return;
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

// snap is the editor as it stands: the text and where the person is standing
// in it, which is the whole of one undo step.
const snap = (area) => ({ text: area.value, start: area.selectionStart, end: area.selectionEnd });

// stepped records what one input did. The kinds are the input's own, so that
// writing and deleting are two runs rather than one, and anything that is
// neither, a drop or a paste this editor did not take over, stands alone rather
// than joining the typing beside it.
function stepped(ed, e) {
  if (!ed.id || (e && e.isComposing)) return;
  const kind = !e ? 'step'
    : e.inputType === 'insertText' || e.inputType === 'insertLineBreak' ? 'type'
      : String(e.inputType).startsWith('delete') ? 'cut' : 'step';
  const now = snap(ed.area);
  undo.of(ed.id, now).record(now, kind, Date.now(), ed.was);
}

// wrote is a write this script made to the textarea rather than a key somebody
// pressed: Enter continuing a list, the head a split keeps, a paste of several
// paragraphs, words put back after a refusal. The state before it and the state
// after it are each a step of their own, so one Ctrl+Z takes back exactly that
// write and nothing on either side of it.
//
// What it never does is unmake the command that went with the write. Undo in
// the block that kept the head of a split puts that block's earlier text back
// and leaves the block below where it is, because that one is the server's to
// make and unmake and its insert is already in the air; a join is the same the
// other way round, and the history of the block that was deleted went with it.
function wrote(id, was, now) {
  if (!id) return;
  const h = undo.of(id, was);
  h.record(was);
  h.record(now);
}

// stepBack is Ctrl+Z and its two redos. It puts a snapshot back into the
// textarea and then goes on through the ordinary path for typed text, so the
// entry holds it and the save that follows is a save like any other. Nothing
// here talks to the server.
//
// A block waiting on a conflict is left where it is. Its snapshots were taken
// against text that does not hold the other person's words, and typing over a
// conflict is what answers it, so an undo there would answer it with text from
// before they wrote anything and put that over them in silence. A refusal is
// not the same: what is typed there is kept against the answer and sends
// nothing, so an undo is as good as any other keystroke.
function stepBack(ed, forward) {
  if (!ed.id) return;
  const w = work.get(ed.id);
  if (w && w.status === 'conflict') return;
  const h = undo.of(ed.id, snap(ed.area));
  const step = forward ? h.redo() : h.undo();
  if (!step) return;
  ed.area.value = step.text;
  ed.area.setSelectionRange(step.start, step.end);
  typed(ed);
  ed.fit();
}

// The keys that do something to the block rather than to the text in it. Each
// one falls through to what a textarea does on its own the moment its
// conditions are not met, so nothing here takes a key away from somebody
// without giving them what it promised instead.
function key(e, ed) {
  // Escape leaves the block. It stops here so that it does not also close the
  // drawer or the palette on its way up.
  if (e.key === 'Escape') { e.stopPropagation(); ed.area.blur(); return; }
  // A key pressed mid composition belongs to the input method.
  if (e.isComposing) return;
  // Undo and redo, taken whether or not this block has anything to give back.
  // The moment anything wrote to this textarea from script the browser's own
  // history went with it, so there is nothing underneath to fall through to and
  // the key is answered here or not at all.
  if ((e.ctrlKey || e.metaKey) && !e.altKey && /^[zy]$/i.test(e.key)) {
    e.preventDefault();
    stepBack(ed, e.shiftKey || e.key.toLowerCase() === 'y');
    return;
  }
  if (e.key === 'Enter' && !e.shiftKey) { pressedEnter(e, ed); return; }
  if (e.key === 'Backspace') { pressedBackspace(e, ed); return; }
  if (e.key === 'ArrowUp' || e.key === 'ArrowDown') pressedArrow(e, ed);
}

// Enter writes the next item of a list into this block, or cuts the block in
// two at the caret. Both are the same with a connection and without one: the
// next item is typing, and the block below is drawn here and made when there is
// somewhere to make it. In a block waiting on an answer it is a plain newline,
// because a block cut in two under a refusal would leave the half below with
// nothing to go back into when the refusal is discarded. Shift+Enter is always
// a plain newline.
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
    const was = snap(ed.area);
    ed.area.value = what.text;
    ed.area.setSelectionRange(what.caret, what.caret);
    wrote(ed.id, was, snap(ed.area));
    typed(ed);
    ed.fit();
    return;
  }
  const w = work.get(ed.id);
  if (w && w.status !== 'ok') return;
  e.preventDefault();
  split(ed, what.before, what.after);
}

// split is Enter in the middle of a block: this one keeps what was in front of
// the caret, and what was behind it becomes the block below. That block is
// drawn at once, with the caret in it, and the command that makes it goes now
// or waits in the outbox. Nothing typed while it waits is lost or lands in the
// wrong block: it is typed into that block's own entry, which writes it into
// the command that has not gone yet or hands it to the real block when the ack
// arrives.
function split(ed, before, after) {
  const id = ed.id;
  const doc = current();
  const whole = snap(ed.area);
  ed.area.value = before;
  wrote(id, whole, snap(ed.area));
  ed.fit();
  typed(ed);
  // The half that stays goes up the ordinary way. leave sends what the entry
  // holds and gives the editor up, which is what has to happen anyway now the
  // caret is moving out of it.
  leave(id);
  startEditing(newBlock(doc.id, id, after, true).id, 0);
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
    const was = snap(editing.area);
    editing.area.value = w.text;
    editing.area.setSelectionRange(join, join);
    wrote(id, was, snap(editing.area));
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
  if (area.selectionStart !== 0 || area.selectionEnd !== 0) return;
  const w = work.get(ed.id);
  if (w && w.status !== 'ok') return;
  const list = neighbours();
  const at = list.findIndex((b) => b.id === ed.id);
  if (at <= 0) return;
  const above = work.get(list[at - 1].id);
  if (above && above.status !== 'ok') return;
  if (ed.id < 0) {
    // A block the server has not made yet is taken back rather than deleted:
    // the words go on the end of the block above and the command that would
    // have made this one leaves the queue, which needs no server at all. Not
    // while another block is waiting to be made under this one, though: that
    // command names this one by key and would have nowhere to go. And not once
    // the command has gone up, which unmake says more about.
    if (named(ed.id)) return;
    e.preventDefault();
    unmake(ed, list[at - 1].id);
    return;
  }
  if (!online()) return;
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

// unmake is the join above for a block that was never made: the command that
// would have made it is dropped and the block comes off the page, so there is
// nothing for the server to hear about at all.
//
// The command has to be one nobody has sent, which is what unqueue answers. One
// the drain has taken, whether it is in the air now or was in the air when a
// socket died, may already have been applied, and taking the block back would
// leave the paragraph in two places, one of them on everybody else's screen. So
// would one that went up on a live socket, which is in no queue to be dropped.
// In both of those this does nothing at all, and the key it was pressed with is
// spent: one dead Backspace while an insert is in the air, rather than a
// paragraph twice.
function unmake(ed, prev) {
  const row = blockAnywhere(ed.id);
  const w = work.get(ed.id);
  const text = w ? w.text : row.text;
  unqueue(row.key).then((gone) => {
    if (!gone || !gone.done) return;
    if (w) { clearTimeout(w.timer); work.delete(ed.id); }
    if (editing === ed) editing = null;
    unmakeLocal(row.key);
    const join = putBack(prev, text);
    if (join >= 0) startEditing(prev, join);
  });
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
    const after = up ? (at > 1 ? list[at - 2].id : 0) : list[at + 1].id;
    // A move is a command about two blocks the server has, and one it has not
    // made yet is neither: it has no id to be moved and none to be moved behind.
    // It moves when it is made, which is where the person put it.
    if (ed.id < 0 || after < 0) return;
    e.preventDefault();
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
// The block holding that remainder is drawn here as one block, uncut, and the
// server's blocks take its place when the insert lands. It is not cut up here
// because the client never imitates the server's rule for cutting; until it is
// made it is an ordinary block of this tab's own and can be typed into, which
// writes the words into the insert that has not gone yet.
//
// A refusal puts the whole of that remainder back on the end of this block,
// through the ordinary entry, so nothing pasted is lost.
function pasted(e, ed) {
  if (!e.clipboardData) return;
  const w = work.get(ed.id);
  if (w && w.status !== 'ok') return;
  const parts = chunks(e.clipboardData.getData('text'));
  if (parts.length < 2) return;
  e.preventDefault();
  const { area } = ed;
  const was = snap(area);
  const rest = parts.slice(1).join('\n\n') + area.value.slice(area.selectionEnd);
  area.value = area.value.slice(0, area.selectionStart) + parts[0];
  area.setSelectionRange(area.value.length, area.value.length);
  wrote(ed.id, was, snap(area));
  typed(ed);
  ed.fit();
  newBlock(current().id, ed.id, rest, false);
}

// The + after the last block is how a document grows a paragraph without being
// in one already, and how an empty document is started. It is quiet until the
// pointer or the focus is on it, like the other affordances.
function addBlock(doc) {
  const last = (doc.blocks || []).at(-1);
  return el('button', {
    type: 'button', class: 'addblk', id: 'blocknew', text: '+', 'aria-label': 'Add a block',
    title: 'Add a block',
    onclick: () => startEditing(newBlock(doc.id, last ? last.id : 0, '', true).id),
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
  // A block waiting on an answer is still a block in an order, and moving one
  // touches neither its text nor its version, so the answer it is waiting for
  // is the same answer wherever it stands.
  if (canEdit()) grip(node, id);
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
    first: 0, timer: 0, flight: false, filed: 0, status: 'ok',
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

// typed is a keystroke in an editor. It is given that editor rather than a
// block id, and reads the textarea that raised the change: the editor this tab
// is in can already be another block's by the time an event from the one before
// it is handled, and writing one block's words into another block's entry is
// how a paragraph is lost.
function typed(ed) {
  const id = ed.id;
  const w = entryOf(id);
  if (!w) return;
  w.text = ed.area.value;
  // A refusal stands until it is answered, and what is typed over it is kept
  // against that answer. Sending again on the next keystroke would be the same
  // text refused for the same reason every second of typing, with a line about
  // it each time; try it again and discard are the two ways out. The words do
  // follow the row that is keeping the question, on the same pause in the
  // typing a save waits for, so that a reload never finds older ones than this
  // tab held. Not on the keystroke: it is a write to a database, and the
  // question is waiting on a person either way.
  if (w.status === 'refused') {
    clearTimeout(w.timer);
    w.timer = setTimeout(() => { w.timer = 0; keepClash(id); }, idle);
    status();
    return;
  }
  if (w.status === 'conflict') {
    // Typing over a conflict is an answer to it: this text, from the version
    // they changed. Without moving the base it would be refused for ever.
    w.base = w.version;
    w.status = 'ok';
    forgetClash(w);
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
  // A block the server has not made yet has no id a save could name, and
  // nothing with a negative one is ever sent. What it has instead is the
  // command that will make it, waiting in the outbox: writing this text into
  // that command is the save, and it is what keeps a block written in with no
  // connection through a reload, because the outbox is on disk and an entry is
  // not. When there is no such command to write into, because it is in the air
  // on a live socket, the text stays in the entry and goes up as an ordinary
  // save the moment the real block takes this row's place.
  if (id < 0) {
    const row = blockAnywhere(id);
    retext(row.key, sent).then((held) => {
      // The row is what the page draws, so it says what the person has written
      // whether or not the command could take it: a paragraph that reads as
      // empty while its words are in the editor is the page lying about them.
      // What the command carries is what the server will make, and a reload
      // draws the row from the command again.
      writeLocal(row, sent);
      if (held && held.done) {
        acked(id, sent, base, null);
        return;
      }
      const entry = work.get(id);
      if (entry) entry.flight = false;
      // This is the moment this tab learns the outbox holds nothing for this
      // block, and the save line reads the count of what it holds. Without a
      // recount here that count is whatever the last one left, which is what
      // was waiting before another tab drained the queue, and the line answers
      // from it instead of saying this block is waiting to save. The line is
      // written again when the count comes back, because the status below runs
      // before it and a render is a frame away. Nothing here arms a timer: the
      // next keystroke is what moves this block on.
      if (held && !held.filed) count().then(status);
      // No command to write into, none in the air under this name and none
      // waiting means the command has been answered and left. Only a tab whose
      // read of the stream has finished since the last moment its socket could
      // have dropped a frame may conclude that: one with no socket, or one
      // whose read has not come back, has heard nothing either way, and another
      // tab of this person emptying the outbox looks exactly like this from
      // here. Such a tab keeps the entry and waits, and the reconnect brings
      // the insert, settles the row and carries the entry to the real block,
      // where the difference goes up as an ordinary save.
      //
      // With the stream read and still no command, nothing is ever coming for
      // this block. The words stay in the entry, and the block says for itself
      // that it was not saved: the two answers a refusal already has, try it
      // again under a name of its own or discard. Neither is this tab's to
      // choose, because the block the first command made may be on the page
      // beside this one.
      if (entry && held && !held.filed && !flying.has(row.key) && caughtUp()) {
        // The name this block was drawn under is spent, so a second attempt has
        // to be a command of its own. The renaming waits for the press: until
        // then the row still answers to the name the event carries, so a
        // refusal declared in error settles on the insert the stream brings
        // late and there is nothing to press Try again into.
        entry.rename = true;
        // What the entry holds now rather than what this save carried: the
        // person may have written more while the read was out, and a refusal
        // answers for everything in the block.
        refusedHere(row, 'the command that would have made this block is gone', entry.text);
      }
      status();
    });
    status();
    return;
  }
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
  // Their words are in this text and in none of the steps taken before it, so
  // undoing to one of those would take their words back out and save that. The
  // history begins again from the merged text.
  //
  // ponytail: the ceiling is that Ctrl+Z does not reach back past somebody
  // else's edit to the same block. The upgrade is to carry every step through
  // the same rebase the textarea's own text goes through here.
  undo.reset(id, r.text, r.caret);
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
  keepClash(id);
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
  keepClash(id);
  notice(id);
  emit();
  status();
}

// The unanswered question, kept where a closed tab cannot take it.
//
// A conflict and a refusal are the same thing to the outbox: a command the
// server would not take, with what the person wrote in it and, for a conflict,
// what the server holds instead. That is the row a command refused during a
// drain becomes, and filing one here makes the two one thing, so the panel
// offers both, either answer settles both, and a reload finds both again.

// clashMessage is what a conflict is called in the panel, which is what the
// socket calls it when it refuses a queued one.
const clashMessage = new Conflict().message;

// keyOf names the question about one block, which is how a second conflict on
// that block writes over the first instead of asking twice. A real block is
// named by the save that was refused, in the same words the outbox folds two
// saves of one block together under; a block this tab drew is named by the key
// its insert goes up under, which is the only name the server has never heard.
const keyOf = (b) => (b.id < 0 ? 'block.insert:' + b.key : target('block.set', { block: b.id }));

// keepClash files the entry's question. The row is the command that was
// refused, as it would have to be sent again, so the panel can send it and
// this module can draw the block from it after a reload.
function keepClash(id) {
  const w = work.get(id);
  const b = w && blockAnywhere(id);
  if (!b || w.status === 'ok') return;
  const clash = w.status === 'conflict';
  const row = b.id < 0
    // spent says the name this block was drawn under has been answered, which
    // is what rename says in the entry. It has to survive with the row: a tab
    // that read this back after a reload would otherwise press try again into
    // a command the server has already done, spend a round trip being told so,
    // and make nothing. The block does arrive, through the answer's key like
    // any other, so nothing is lost by it; it is a question asked twice for
    // no reason, and a new name is what makes the second one a command.
    ? { proposition: state.open, me: state.me, cmd: 'block.insert', idem: b.key,
      args: insertArgs(b, w.text), base: null, base_text: null, spent: Boolean(w.rename) }
    : { proposition: state.open, me: state.me, cmd: 'block.set', idem: newKey(),
      args: { block: id, text: w.text, base: w.base, whole: true },
      base: w.base, base_text: w.sent };
  file(row, keyOf(b), clash ? clashMessage : w.reason,
    clash ? { entity: 'block', entity_id: id, field: 'text', version: w.version, current: w.theirs } : null)
    .then((n) => {
      if (!n) return;
      // Answered while the write was in the air: typing over a conflict is an
      // answer, and so is somebody else's change arriving that says what this
      // tab says. Either leaves a row nobody is going to be asked about.
      if (work.get(id) === w && w.status !== 'ok') { w.filed = n; count(); } else chosen(n);
    });
}

// forgetClash drops the row because the question has been answered here.
function forgetClash(w) {
  if (!w || !w.filed) return;
  chosen(w.filed);
  w.filed = 0;
}

// unanswered is the two ways a filed question and this tab's entry are kept
// saying the same thing, walked on every render because that is when both have
// just been read: the rows by count() and the blocks by the stream.
//
// A row nothing here is answering draws its block, which is how a conflict
// comes back after a reload, how one refused during a drain reaches the block
// it is about at all, and how the other tab of this browser hears about it.
// An entry whose row has gone was answered somewhere else, in the panel or in
// that other tab, and it is put right here so that nothing is ever asked twice
// and no block goes on asking a question that has been settled.
//
// It is a deleter of an entry, which only finished, the two answers, prune and
// a refusal for a block that is gone otherwise are: it is those same two
// answers, arriving from somewhere other than this block.
function unanswered() {
  for (const row of state.refused) {
    const b = blockFor(row);
    if (!b) continue;
    const w = work.get(b.id);
    // A block this tab is already answering for, or one somebody is typing in
    // with nothing outstanding: the entry is what this tab is going by, and the
    // row is the same question written down. The entry of a block being typed
    // in stops the question being drawn over the caret; it is drawn the moment
    // that entry is finished with.
    if (w) { if (w.status !== 'ok') w.filed = row.n; continue; }
    work.set(b.id, entryFrom(b, row));
    notice(b.id);
  }
  for (const [id, w] of work) {
    if (w.status === 'ok' || !w.filed) continue;
    if (state.refused.some((r) => r.n === w.filed)) continue;
    if (id < 0) {
      // A block this tab drew, whose insert was let go, has already left the
      // page: letGo unmakes it, prune takes the entry, and this never sees it.
      // So what is left here is the panel's try it again, which puts the same
      // insert back in the queue. The block stands and stops asking.
      w.filed = 0;
      w.status = 'ok';
      delete w.reason;
      notice(id);
      arm(id, w);
      continue;
    }
    takeTheirs(id);
  }
}

// blockFor is the block a refused row is about: the one a save names, or the
// one this tab drew for an insert, which the key on the command names.
function blockFor(row) {
  if (row.proposition !== state.open) return null;
  if (row.cmd === 'block.set') return blockAnywhere(row.args.block);
  if (row.cmd === 'block.insert') return localOf(row.idem);
  return null;
}

// entryFrom is the entry a filed row is read back into, in the state it was in
// when it was filed. base and sent are what the command was measured from, so
// that keep mine sends the same change it would have sent an hour ago, and the
// server merges it into whatever the block has become since.
function entryFrom(b, row) {
  const w = {
    text: row.args.text ?? b.text,
    base: row.args.base ?? b.version,
    sent: row.cmd === 'block.insert' ? row.args.text : (row.base_text ?? b.text),
    first: 0, timer: 0, flight: false, filed: row.n,
    status: row.detail ? 'conflict' : 'refused', reason: row.refused,
  };
  if (row.spent) w.rename = true;
  if (row.detail) {
    w.theirs = row.detail.current;
    w.version = row.detail.version;
  }
  return w;
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
  // The timer just cleared may have been the one carrying what was typed over
  // a refusal into the row that is keeping it. Nothing is sent for a block
  // waiting on an answer; this is a write to this device and not to the server.
  else if (w.status !== 'ok') keepClash(id);
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
      forgetClash(w);
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
    let caret = b.text.length;
    if (openOn(id)) {
      caret = Math.min(editing.area.selectionStart, b.text.length);
      editing.area.value = b.text;
      editing.area.setSelectionRange(caret, caret);
      editing.fit();
    }
    // Somebody else's text arriving whole, which is the merge above by another
    // road: every step in the history predates it, so the history starts again
    // from what the block now says. Taking a run of saves back from the
    // activity panel arrives here too, in a tab that has the block open and
    // nothing unsaved in it.
    undo.reset(id, b.text, caret);
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
// What cannot go is what was typed into a block the server has not made yet:
// there is no id to address a save to, and the command that would carry it is
// in a database this page has no time left to write to. The loop below says so
// where it steps over one. How much that is depends on where the command is: a
// block whose insert is still in the outbox has everything up to the last save
// in it, so at most the timer's ceiling is lost, while one whose insert has
// gone up and not been answered has nothing of what was typed since, which is
// the whole of that flight.
addEventListener('pagehide', () => {
  if (editing) {
    const w = work.get(editing.id);
    if (w) w.text = editing.area.value;
  }
  for (const [id, w] of work) {
    if (w.status === 'conflict' || w.text === w.sent) continue;
    // A block the server has not made yet is saved by writing into the command
    // that will make it, and that is a database this page has no time left to
    // write to.
    if (id < 0) continue;
    const base = w.status === 'refused' ? blockAnywhere(id)?.version ?? w.base : w.base;
    send('block.set', { block: id, base, text: w.text, whole: true }).catch(() => {});
  }
});

// choice is the notice above a block the server would not take: what happened,
// and the two answers. Both kinds come to the same pair, send what was written
// or let it go, so both are the same two buttons under different names.
// What they wrote is read off the block rather than out of the entry, because
// a conflict this tab has been holding since before a reload may be hours old
// and the block may have moved on twice since. Take theirs gives what the block
// holds now, so that is what this has to be offering. The two are the same text
// on a conflict that has just happened: the refusal carried what the server
// held and drawing it put it on the row.
function choice(id) {
  const w = work.get(id);
  const theirs = blockAnywhere(id)?.text ?? w.theirs;
  const said = w.status === 'refused'
    ? [el('span', { text: 'That was not saved: ' + w.reason })]
    : [el('span', { text: 'Somebody changed this block while you were writing. It now reads: ' }),
      el('span', { class: 'theirs', text: theirs || '(nothing)' })];
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
  forgetClash(w);
  if (give) {
    takeTheirs(id);
    return;
  }
  w.base = w.status === 'conflict' ? w.version : b.version;
  w.status = 'ok';
  notice(id);
  // Trying again with a block that was never made is the command that makes it
  // going again, carrying whatever has been typed into it since. A block whose
  // first command was answered and gone is renamed here, at the press, because
  // under the name it was drawn with the server would say what that command did
  // rather than making anything. A command the server refused keeps its name:
  // it spent nothing, and the insert waiting underneath this block names it.
  if (id < 0) {
    const fresh = w.rename ? rekeyLocal(b, newKey()) || b : b;
    delete w.rename;
    insert(fresh);
    return;
  }
  emit();
  save(id);
  status();
}

// takeTheirs is the block left standing as the document holds it: the entry
// goes, and an editor open on it is refilled from the block, so nothing
// anybody is looking at still shows words the document does not have.
//
// What they are given is what the block says now, not what it said when the
// clash happened: it may have moved again since, and handing back the older
// text would write over that third change without anybody being asked.
function takeTheirs(id) {
  const w = work.get(id);
  const b = blockAnywhere(id);
  if (!w || !b) return;
  clearTimeout(w.timer);
  work.delete(id);
  // A block the server would not make has no text to fall back to, because
  // there is no row anywhere but here. Letting it go takes it off the page,
  // and the render that follows takes its history with it.
  if (id < 0) {
    if (openOn(id)) editing = null;
    unmakeLocal(b.key);
    emit();
    status();
    return;
  }
  // Take theirs gives this person's text up, and how they got to it with it:
  // an undo back into it would put it over them again without their being
  // asked a second time. Keep mine, in resume above, keeps the history,
  // because the text it keeps is the text every step in it was taken against.
  // A conflict rebuilt after a reload has no history at all, and this gives it
  // one that starts at what the block now reads.
  undo.reset(id, b.text);
  if (openOn(id)) {
    const fresh = entryOf(id);
    editing.area.value = fresh.text;
    editing.area.setSelectionRange(fresh.text.length, fresh.text.length);
    editing.fit();
    notice(id);
  }
  emit();
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
