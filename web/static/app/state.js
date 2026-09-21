// The state the page draws itself from. It starts as the payload the server
// rendered into the page and is moved forward by applied events, whether they
// came back from this tab's own command or arrived from somebody else's.

import * as api from './api.js';
import * as offline from './offline.js';

export const state = {
  me: 0,
  users: new Map(),
  byHandle: new Map(),
  workspace: '',
  // The workspace's IANA time zone. A due date is a calendar day, and whose
  // day it has to have passed is the show's question rather than this laptop's.
  timezone: '',
  statuses: [],
  questions: [],
  questionLabels: [],
  props: [],
  open: 0,
  columns: [],
  cards: new Map(),
  seq: 0,
  presence: [],
  can: {},
  documents: [],
  document: 0,
  docSource: false,

  tab: 'board',
  railFilter: null,
  boardFilter: 'all',
  openCard: null,
  connected: false,
  // conflict is the stale text edits the server refused, by the field each was
  // on: the card it was on and the command that would send it again. It is kept
  // here rather than beside the node the edit was typed in, because the refusal
  // redraws the drawer and takes that node away with it. By field, because a
  // title and a description can each be waiting on a choice at the same time.
  conflict: {},

  // The links and files beside the board. They are fetched when the tab is
  // first opened rather than rendered into the page, because the board is what
  // a reload is for and these are two requests away.
  links: [],
  files: [],
  attachments: { links: [], files: [] },
  loaded: 0,
  openLink: null,
  openFile: null,
  linkKind: 'all',
  linkQuestion: null,
  linkQuery: '',
  folders: [],
  kinds: [],
  folder: 'all',
  fileQuery: '',
  // uploads is what is in flight in this tab, by file id: a fraction and,
  // when one went wrong, what to say about it. A file dropped with nothing to
  // upload it to waits in here under a negative id until there is.
  uploads: new Map(),

  // The offline half. fromCache is a page the service worker handed back with
  // no server behind it; waiting is how many commands the outbox is holding;
  // panel is the activity drawer, with its rows and the refused replays.
  fromCache: false,
  waiting: 0,
  waitingHere: 0,
  panel: false,
  activity: [],
  refused: [],
  // materialFailed is the last read of the links and files having gone wrong,
  // which the panes say out loud rather than reporting there are none.
  materialFailed: false,
};

export function boot(payload) {
  state.me = payload.me;
  state.workspace = payload.workspace;
  state.timezone = payload.timezone || '';
  state.statuses = payload.statuses || [];
  state.questions = payload.questions || [];
  state.questionLabels = payload.question_labels || [];
  state.can = payload.can || {};
  state.presence = payload.presence || [];
  for (const u of payload.users || []) {
    state.users.set(u.id, u);
    state.byHandle.set(u.handle, u);
  }
  state.props = payload.propositions || [];
  sortProps();
  state.open = payload.open || 0;
  loadBoard(payload.board);
  loadDocuments(payload.documents);
  // Opening a proposition is enough to have it on this device. Without this the
  // snapshot was only ever written by an applied event, so a proposition that
  // was read and not edited had nothing cached and said so when the connection
  // went. A page booted from a snapshot is already one and writes nothing.
  remember();
}

function loadDocuments(documents) {
  state.documents = documents || [];
  state.documents.sort(order);
  state.document = state.documents.length ? state.documents[0].id : 0;
  state.docSource = false;
}

function loadBoard(board) {
  state.columns = board ? board.columns || [] : [];
  state.cards = new Map();
  for (const c of (board ? board.cards || [] : [])) state.cards.set(c.id, c);
  state.seq = board ? board.seq || 0 : 0;
}

// material reads the links and files of the open proposition, once. The board
// is rendered into the page because that is what a reload is for; these are two
// requests away and only the tab that shows them needs them. Events keep them
// up to date from then on.
export async function material() {
  if (!state.open || state.loaded === state.open) return;
  // With no connection there is nothing to read them from, and every render
  // would try again and say so again over whatever else is on the bar. What
  // was cached is already here; the next render with a network fetches.
  //
  // The browser's own flag is not believed on its own, because it is wrong
  // often enough to matter: some VPN and captive states report no network while
  // the socket is plainly carrying one, and a pane that trusted the flag would
  // say there are no links for as long as the lie lasted. A live socket is the
  // better witness, and when it says yes the rows are read whatever the flag
  // feels.
  if (!navigator.onLine && !state.connected) return;
  // A read that failed is tried again, because a moment of trouble that nothing
  // else notices should not leave the pane saying there is nothing there for the
  // rest of the session. Not on every render though: ten seconds between goes is
  // often enough to catch a server coming back and rare enough to be quiet.
  if (Date.now() - lastTried < retryAfter) return;
  lastTried = Date.now();
  const proposition = state.open;
  state.loaded = proposition;
  try {
    const [links, files, attached] = await Promise.all([
      api.get('/links?proposition=' + proposition),
      api.get('/files?proposition=' + proposition),
      api.get('/attachments?proposition=' + proposition),
    ]);
    // A tab that was navigated away while these were in flight keeps what it
    // has: the answers belong to a proposition it is no longer showing.
    if (state.open !== proposition) return;
    state.links = links.links || [];
    state.files = files.files || [];
    state.folders = files.folders || [];
    state.kinds = links.kinds || [];
    state.attachments = { links: attached.links || [], files: attached.files || [] };
    state.materialFailed = false;
    // This is the only thing that knows the links and files of a proposition,
    // so it is the only thing that writes them.
    offline.keepMaterial({
      proposition,
      at: Date.now(),
      v: VERSION,
      links: state.links,
      files: state.files,
      folders: state.folders,
      kinds: state.kinds,
      attachments: state.attachments,
    });
    emit();
  } catch (err) {
    // The mark is cleared so another go is possible, and the stamp above is
    // what keeps that from being every render. The pane says it could not read
    // them rather than saying there are none.
    state.loaded = 0;
    state.materialFailed = true;
    // Drawn again now, so the pane says it could not read them rather than
    // keeping the line it was drawn with, which was that there are none. Without
    // this it said the wrong thing for the whole of the gap below.
    emit();
    // One render once the gap has passed, because a pane nobody is touching
    // produces no renders and would otherwise sit on a moment of trouble until
    // somebody clicked something. That render calls this again; if it works
    // there is no failure to schedule another, so this stops on its own.
    if (!nudging) nudging = setTimeout(() => { nudging = 0; emit(); }, retryAfter);
    throw err;
  }
}

// retryMaterial forgets that a read failed, which the socket coming back does:
// that is the event that makes another go worth making right now rather than in
// ten seconds.
export function retryMaterial() {
  state.loaded = 0;
  lastTried = 0;
}

let lastTried = 0;
let nudging = 0;
const retryAfter = 10000;

export function user(id) {
  return state.users.get(id) || { id, name: 'Someone', initials: '??', colour: 'c8', handle: '' };
}

export function byHandle(handle) {
  return state.byHandle.get(handle) || null;
}

export function proposition(id) {
  return state.props.find((p) => p.id === id) || null;
}

export function open() {
  return proposition(state.open);
}

// canEdit is what the board allows right now. The role has to allow it and
// the open proposition has to not be archived, because an archived one is read
// only: every command on it but restore and delete is refused, so drawing the
// controls would only offer a refusal.
export function canEdit() {
  const p = open();
  return Boolean(state.can.edit && !readOnly && p && !p.archived_at);
}

// readOnly is a proposition this device kept a copy of but is not the one it
// saw last, which the plan lets somebody read offline and not change. It is
// held here rather than written into state.can, because state.can goes into the
// snapshot: a tab that wrote it there would tell the next one this person may
// not edit this proposition at all, and there is nothing to take that back.
let readOnly = false;

export function archived() {
  const p = open();
  return Boolean(p && p.archived_at);
}

export function columnCards(columnID) {
  return [...state.cards.values()]
    .filter((c) => c.column_id === columnID)
    .sort(order);
}

export const order = (a, b) => (a.position < b.position ? -1 : a.position > b.position ? 1 : 0);

function sortProps() {
  state.props.sort(order);
}

const listeners = new Set();
let frame = 0;
let held = false;

export function subscribe(fn) {
  listeners.add(fn);
}

// emit coalesces into one frame, and holds off entirely while an inline editor
// is open, so a note arriving from somebody else does not take the field out
// from under the person typing in it.
export function emit() {
  if (frame) return;
  frame = requestAnimationFrame(() => {
    frame = 0;
    if (held) return;
    for (const fn of listeners) fn();
  });
}

export function hold(on) {
  held = on;
  if (!on) emit();
}

// apply moves the state forward by one event. Every payload is the whole row as
// the server has it, so applying an event twice is applying it once.
export function apply(ev) {
  if (!ev) return;
  if (ev.seq > state.seq && ev.proposition === state.open) state.seq = ev.seq;
  const now = ev.after || null;
  const was = ev.before || null;

  switch (ev.entity) {
    case 'proposition':
      if (ev.action === 'delete') {
        state.props = state.props.filter((p) => p.id !== ev.entity_id);
      } else if (now) {
        const i = state.props.findIndex((p) => p.id === now.id);
        if (i < 0) state.props.push(now); else state.props[i] = now;
        sortProps();
      }
      break;

    case 'member': {
      const row = now || was;
      const p = proposition(row.proposition_id);
      if (!p) break;
      const members = new Set(p.members || []);
      if (now) members.add(row.user_id); else members.delete(row.user_id);
      p.members = [...members].sort((a, b) => a - b);
      break;
    }

    case 'column': {
      if (ev.proposition !== state.open) break;
      if (ev.action === 'delete') {
        state.columns = state.columns.filter((c) => c.id !== ev.entity_id);
        break;
      }
      const i = state.columns.findIndex((c) => c.id === now.id);
      if (i < 0) state.columns.push(now); else state.columns[i] = now;
      state.columns.sort(order);
      break;
    }

    case 'card':
      if (ev.proposition !== state.open) break;
      if (ev.action === 'delete') {
        state.cards.delete(ev.entity_id);
        if (state.openCard === ev.entity_id) {
          state.openCard = null;
          state.conflict = {};
        }
      } else {
        state.cards.set(now.id, now);
      }
      break;

    case 'document': {
      if (ev.proposition !== state.open) break;
      if (ev.action === 'delete') {
        state.documents = state.documents.filter((d) => d.id !== ev.entity_id);
        if (state.document === ev.entity_id) {
          state.document = state.documents.length ? state.documents[0].id : 0;
        }
        break;
      }
      // A document event carries the row alone. The blocks a new document
      // starts from arrive as their own inserts, in the same transaction, so a
      // document seen for the first time starts empty and fills.
      const at = state.documents.findIndex((d) => d.id === now.id);
      if (at < 0) state.documents.push({ blocks: [], ...now });
      else state.documents[at] = { ...state.documents[at], ...now };
      state.documents.sort(order);
      break;
    }

    case 'block': {
      if (ev.proposition !== state.open) break;
      const row = now || was;
      const doc = state.documents.find((d) => d.id === row.document_id);
      if (!doc) break;
      const blocks = (doc.blocks ||= []);
      const at = blocks.findIndex((b) => b.id === row.id);
      // A deleted block is a tombstone, so it leaves the list rather than
      // sitting in it with a date on it; undo sends it back without one.
      if (!now || now.deleted_at) {
        if (at >= 0) blocks.splice(at, 1);
        break;
      }
      if (at < 0) blocks.push(now); else blocks[at] = now;
      blocks.sort(order);
      // The real block is here, so the one this tab drew in its place goes, in
      // the same tick and before anything is drawn again. This is the only
      // place that does it, so it happens whichever road the block came by: the
      // answer to this tab's own command, the copy the room was sent, or the
      // stream a tab reads after being away. A tab that did not send the
      // command matches it the same way, which is what stops the paragraph
      // standing twice in a second tab until it reloads.
      if (ev.key) settle(ev.key, now);
      break;
    }

    case 'link':
    case 'file': {
      if (ev.proposition !== state.open) break;
      const list = ev.entity === 'link' ? 'links' : 'files';
      if (!now) {
        state[list] = state[list].filter((x) => x.id !== ev.entity_id);
        if (ev.entity === 'link' && state.openLink === ev.entity_id) state.openLink = null;
        if (ev.entity === 'file' && state.openFile === ev.entity_id) state.openFile = null;
        break;
      }
      const at = state[list].findIndex((x) => x.id === now.id);
      if (at < 0) state[list].unshift(now); else state[list][at] = now;
      break;
    }

    case 'card_link':
    case 'card_file': {
      if (ev.proposition !== state.open) break;
      const row = now || was;
      const list = ev.entity === 'card_link' ? 'links' : 'files';
      const key = ev.entity === 'card_link' ? 'link_id' : 'file_id';
      const joins = state.attachments[list];
      const at = joins.findIndex((j) => j.card_id === row.card_id && j[key] === row[key]);
      if (!now) { if (at >= 0) joins.splice(at, 1); }
      else if (at < 0) joins.push(row);
      break;
    }

    case 'checklist_item':
    case 'comment': {
      const row = now || was;
      const card = state.cards.get(row.card_id);
      if (!card) break;
      const list = ev.entity === 'comment' ? (card.comments ||= []) : (card.checklist ||= []);
      const at = list.findIndex((x) => x.id === row.id);
      if (!now) { if (at >= 0) list.splice(at, 1); }
      else if (at < 0) list.push(now);
      else list[at] = now;
      break;
    }
  }
  remember();
  emit();
}

// The optimistic half. Every command the browser sends is drawn before the
// server has answered and reconciled when it does: the echo carries the whole
// row, so applying it after the guess is applying the guess again, and a
// refusal puts back what was there.
//
// Only the commands that change a row already on the screen are in here. A
// create has no id to draw under until the server has given it one, and waiting
// the width of a socket round trip for one is not something anybody notices.
//
// last says the command sets a field to a value, so two of them on one row are
// the second one. Those are the ones the outbox folds together while there is
// nothing to send them to. Assigning is not one of them: two assignments are
// two people.
const optimistic = {
  'card.title': { entity: 'card', id: (a) => a.card, last: true, fields: (a) => ({ title: a.title }) },
  'card.description': { entity: 'card', id: (a) => a.card, last: true, fields: (a) => ({ description_md: a.text }) },
  'card.due': { entity: 'card', id: (a) => a.card, last: true, fields: (a) => ({ due_date: a.due }) },
  'card.question': { entity: 'card', id: (a) => a.card, last: true, fields: (a) => ({ question: a.question }) },
  'card.done': { entity: 'card', id: (a) => a.card, last: true, fields: (a) => ({ done_at: a.done ? seconds() : null }) },
  'card.assign': {
    entity: 'card', id: (a) => a.card,
    fields: (a, row) => ({ assignees: [...new Set([...(row.assignees || []), a.user])] }),
  },
  'card.unassign': {
    entity: 'card', id: (a) => a.card,
    fields: (a, row) => ({ assignees: (row.assignees || []).filter((id) => id !== a.user) }),
  },
  'card.move': {
    entity: 'card', id: (a) => a.card, last: true,
    fields: (a, row) => ({ column_id: a.column, position: behind('card', a.after, row) }),
  },
  'block.move': {
    entity: 'block', id: (a) => a.block, last: true,
    fields: (a, row) => ({ position: behind('block', a.after, row) }),
  },
  'column.rename': { entity: 'column', id: (a) => a.column, last: true, fields: (a) => ({ name: a.title }) },
  'checklist.toggle': { entity: 'checklist_item', id: (a) => a.item, last: true, fields: (a) => ({ done: a.done }) },
  'block.set': { entity: 'block', id: (a) => a.block, last: true, fields: (a) => ({ text: a.text }) },
  'proposition.status': { entity: 'proposition', id: (a) => a.proposition, last: true, fields: (a) => ({ status: a.status }) },
  'proposition.edit': {
    entity: 'proposition', id: (a) => a.proposition, last: true,
    fields: (a) => ({ title: a.title, statement: a.statement, blurb: a.blurb }),
  },
};

const seconds = () => Math.floor(Date.now() / 1000);

// behind is a position key that sorts just after the row the drop landed on, a
// card in a column or a block in a document. The server never writes a key that
// ends in a zero, so that every fraction has one spelling and there is always
// room directly below one; a zero on the end of the row it landed on is
// therefore above that row and below every key the server can write above it,
// wherever the neighbor above turns out to lie. A drop at the head of a list is
// the empty key, which is below every key there can be.
//
// ponytail: two rows dropped into one gap before either is acked are drawn on
// the same key, and which of them is above the other is settled only when the
// acks arrive. The upgrade is the server's own key generator here, so that the
// second guess is made between the first one and the row below it.
function behind(entity, after, row) {
  if (!after) return '';
  const previous = rowOf(entity, after);
  return previous ? previous.position + '0' : row.position;
}

function rowOf(entity, id) {
  switch (entity) {
    case 'card':
      return state.cards.get(id) || null;
    case 'column':
      return state.columns.find((c) => c.id === id) || null;
    case 'proposition':
      return proposition(id);
    case 'checklist_item':
      for (const card of state.cards.values()) {
        const item = (card.checklist || []).find((i) => i.id === id);
        if (item) return item;
      }
      return null;
    case 'block':
      for (const doc of state.documents) {
        const block = (doc.blocks || []).find((b) => b.id === id);
        if (block) return block;
      }
      return null;
    default:
      return null;
  }
}

// predict draws one row as it will be, through the same path an event takes so
// that there is one way into the state and not two. It answers the function
// that puts the row back, which is what a refusal calls.
export function predict(cmd, args) {
  const spec = optimistic[cmd];
  if (!spec) return null;
  const row = rowOf(spec.entity, spec.id(args));
  if (!row) return null;
  const was = { ...row };
  const touched = Object.keys(spec.fields(args, row));
  apply(local(spec.entity, { ...row, ...spec.fields(args, row) }));
  // Undrawing puts back the fields the guess touched and leaves the rest of the
  // row where it is. Starting from the copy taken before the guess would undo
  // whatever else has happened to that row since, and while a command was in
  // the outbox somebody may well have changed a field beside it. A conflict
  // does better still on the one field it is about: it carries what the server
  // holds and the version it holds it at, which is where the row actually is.
  return (detail) => {
    const now = rowOf(spec.entity, spec.id(args));
    // A revert puts fields back on a row. It never brings a row back: one that
    // has gone was deleted while this was in flight, and drawing the copy taken
    // before the guess would put it on this screen and no other, where it would
    // stay until a reload and be refused every time anybody wrote to it.
    if (!now) return;
    const back = { ...now };
    for (const field of touched) back[field] = was[field];
    if (detail && detail.field) {
      back[detail.field] = detail.current;
      back.version = detail.version;
    }
    apply(local(spec.entity, back));
  };
}

// local is an event this tab made up. Sequence zero, so it never moves the
// stream's place: the real one arrives with a number on it.
function local(entity, row) {
  return { seq: 0, proposition: state.open, entity, entity_id: row.id, action: 'edit', after: row };
}

// The blocks this tab has made that the server has not. Each is a row in its
// document like any other, so everything that draws or walks a document sees
// it, with a negative id and three fields of its own: the key the insert that
// makes it goes up under, and where that insert says it goes, which is a block
// id or another block's key. They are written into the snapshot with the rest,
// which is what keeps a block made with no connection on the page across a
// reload. Nothing with a negative id is ever sent; docs.js is the one place
// that knows how to name one to the server.
export function makeLocal(row) {
  let id = 0;
  for (const doc of state.documents) for (const b of doc.blocks || []) id = Math.min(id, b.id);
  const made = {
    ...row, id: id - 1, position: under(row.document_id, row.after), version: 0,
    updated_by: state.me, updated_at: seconds(),
  };
  apply(local('block', made));
  // Written now rather than in two seconds, because this row is in this tab and
  // nowhere else: a reload before the timer fires would lose the paragraph off
  // the page, and with no connection there is no server to draw it again from.
  write();
  return made;
}

// under is where a block this tab has made is drawn: behind the block it was
// made under, by the rule behind above follows. Nothing at all is the head of
// the document, which is where a block made under nothing goes.
//
// A block made under one this tab does not hold goes at the end of its document
// instead. That is an insert whose anchor has already been answered and left
// the outbox, so the block it names is on the server and this one belongs after
// it; the head, which is where an anchor nobody can find would otherwise put
// it, is the one place it certainly does not belong.
function under(document, after) {
  const above = after ? rowOf('block', after) : null;
  if (above) return behind('block', after, above);
  if (!after) return '';
  const blocks = (state.documents.find((d) => d.id === document) || {}).blocks || [];
  const last = blocks[blocks.length - 1];
  return last ? last.position + '0' : '';
}

// writeLocal is typing reaching the row itself. A block the server holds is
// written by the event its save comes back as; one it does not hold has no such
// event, and without this a reload would draw it as it was first made.
export function writeLocal(row, text) {
  // Not once the real block has taken its place, which a save that was in the
  // air while the answer arrived would otherwise do: applying a row that is no
  // longer in the document puts it back, and the paragraph would stand twice
  // for the rest of the session.
  if (!localOf(row.key)) return;
  apply(local('block', { ...row, text, updated_at: seconds() }));
  // Written now for the reason a new one is: this text is in the command the
  // outbox holds and in this tab, and nowhere a reload could read it from
  // otherwise. It is the save rate rather than the typing rate, because the
  // caller writes here when the text reaches that command and not before.
  write();
}

// settle is the real block taking the place of the one this tab drew for it.
// The row goes here rather than through unmakeLocal below, because this runs
// inside the apply that has just drawn the real one and the two must be one
// change: the paragraph is never on the page twice, not even for a frame, and
// never off it for one either.
//
// A command that made several blocks, which is a paste the server cut up, spent
// the key and then the key with a number on the end. The row this tab drew
// stood for all of them, so it goes when the first arrives and the rest find
// nothing to settle, which is why the number is cut off before the lookup.
function settle(key, now) {
  const made = localOf(key.split('#')[0]);
  if (!made) return;
  for (const doc of state.documents) {
    const at = (doc.blocks || []).indexOf(made);
    if (at >= 0) doc.blocks.splice(at, 1);
  }
  // Written now for the reason making one is: a reload before the timer fires
  // would draw the block the server has and this one standing for it.
  write();
  if (settled) settled(made, now);
}

// settleReplayed is the answer to a command the server had already applied. It
// is not drawn, because the row it carries may be older than what this tab
// holds, and the row it is about is on the page already: it came in the payload
// this tab loaded, or through the stream. What is left is the block this tab
// drew for that command, still standing beside the real one, and nothing else
// will ever come to put the two together.
export function settleReplayed(ev) {
  if (!ev || !ev.key || ev.entity !== 'block') return;
  settle(ev.key, rowOf('block', ev.entity_id) || ev.after);
}

// settled is how docs.js hears it, handed here rather than imported because
// this module knows nothing of the editor: the text somebody has typed into
// such a block, and an editor standing in it, follow the row to the real block.
let settled = null;

export function onSettled(fn) {
  settled = fn;
}

// rekeyLocal gives a block this tab drew a new name, which is what Try again
// sends the second attempt under. The command that was to make it under the old
// one has been answered and gone, so the attempt has to be a command of its
// own: sent under a name the server has already answered it would be told what
// that answer was rather than making anything.
//
// The name it was drawn under stays on the row as former. An insert waiting to
// be made under this block was filed against that one and is left alone, since
// on the server that name still points at the block the first command made,
// which is where it belongs; this is what the page reads it back by.
export function rekeyLocal(row, key) {
  apply(local('block', { ...row, key, former: row.key }));
  return localOf(key);
}

// localOf is the block a key names: what a refusal, a block joined back into
// the one above it and an insert queued behind it all find the row again by.
export function localOf(key) {
  for (const doc of state.documents) {
    const row = (doc.blocks || []).find((b) => b.id < 0 && b.key === key);
    if (row) return row;
  }
  return null;
}

// drawQueued puts the page and the outbox back in step over the blocks this
// device has made and not sent. The snapshot is a drawing of what one tab had;
// the outbox is the record of what this device promised, and it is the only one
// of the two that is shared, so the drawing can be older than the promise or
// missing it altogether: another tab that never had these rows writes a
// snapshot without them, and a reload then loses the paragraph off the page
// while its command is still waiting to go.
//
// So every command that is still waiting draws its block, from its own
// arguments, and a block drawn for a command that is no longer there goes: a
// command leaves the outbox when it has been answered, and the block it made is
// then a real one. The commands are walked in the order they were filed, which
// is the order they will go up in, so one that names another by key finds the
// block it names already drawn.
export async function drawQueued() {
  if (!state.open) return;
  // A read that did not happen is not an outbox with nothing in it. Taken as
  // one, the walk at the foot of this function undraws every block this device
  // has promised and not sent, on the grounds that it found no command for any
  // of them, and the paragraphs leave the page while their commands sit in a
  // store nobody could open.
  const rows = await offline.queued();
  if (!rows) return;
  const mine = new Map();
  for (const doc of state.documents) {
    for (const b of doc.blocks || []) if (b.id < 0 && b.key) mine.set(b.key, b);
  }
  const waiting = new Set();
  for (const row of rows) {
    // A command the server would not take keeps its block too. The panel holds
    // the row with the two answers and undraws the block by its key when the
    // person lets it go, so a reload that drew everything but the refused ones
    // would take the paragraph off the page and leave the answer to a question
    // about nothing.
    if (row.cmd !== 'block.insert') continue;
    if (row.me && state.me && row.me !== state.me) continue;
    const args = row.args || {};
    if (!state.documents.some((d) => d.id === args.document)) continue;
    waiting.add(row.idem);
    const drawn = mine.get(row.idem);
    if (drawn) {
      if (drawn.text !== args.text) apply(local('block', { ...drawn, text: args.text }));
      continue;
    }
    // A command that names where it goes by key names a block this tab drew.
    // If that block is gone the command it named has been answered, so the
    // block it made is on the server and this one belongs at the end of the
    // document rather than at the head, which is where an anchor nobody can
    // find would otherwise put it.
    const above = args.after_key ? mine.get(args.after_key) : null;
    const blocks = (state.documents.find((d) => d.id === args.document) || {}).blocks || [];
    const last = args.after_key && !above ? blocks[blocks.length - 1] : null;
    mine.set(row.idem, makeLocal({
      document_id: args.document, text: args.text, whole: Boolean(args.whole), key: row.idem,
      after: (above && above.id) || (last && last.id) || args.after || 0,
      to: args.after_key ? { after_key: args.after_key } : { after: args.after || 0 },
    }));
  }
  for (const [key, row] of mine) if (!waiting.has(key)) unmakeLocal(key);
}

// unmakeLocal takes one off the page when no block is ever going to arrive for
// it: an insert the server would not take, either answered on the block itself
// or let go from the activity panel, and one joined back into the block above
// before it was made at all. The real block arriving is settle above.
export function unmakeLocal(key) {
  const row = localOf(key);
  if (row) {
    apply({ seq: 0, proposition: state.open, entity: 'block', entity_id: row.id,
      action: 'delete', before: row });
    // For the reason above: a reload before the timer fires would draw a block
    // the server has, and this one that stands for the same paragraph.
    write();
  }
  return row;
}

// target names the row and the field a command sets, or nothing for a command
// that adds rather than sets. It is what the outbox folds two edits together
// on, so a second edit to a title made with no connection replaces the first
// rather than queueing behind it and going up against a version the server has
// already moved past.
export function target(cmd, args) {
  const spec = optimistic[cmd];
  if (!spec || !spec.last) return '';
  const id = spec.id(args);
  return id ? `${cmd}:${id}` : '';
}

// baseText is what the field held before the change. It is kept beside a queued
// command so a refusal an hour later can still say what this device was working
// from, and it has to be read before the guess is applied.
export function baseText(cmd, args) {
  const spec = optimistic[cmd];
  if (!spec) return null;
  const row = rowOf(spec.entity, spec.id(args));
  if (!row) return null;
  if (cmd === 'block.set') return row.text || '';
  if (cmd === 'card.title') return row.title || '';
  if (cmd === 'card.description') return row.description_md || '';
  return null;
}

// The snapshot is written in the shape the server renders into the page, so a
// page the worker served with no payload in it boots from this unchanged.
function snapshot() {
  return {
    me: state.me,
    workspace: state.workspace,
    timezone: state.timezone,
    statuses: state.statuses,
    questions: state.questions,
    question_labels: state.questionLabels,
    can: state.can,
    presence: [],
    users: [...state.users.values()],
    propositions: state.props,
    open: state.open,
    board: { columns: state.columns, cards: [...state.cards.values()], seq: state.seq },
    documents: state.documents,
  };
}

// VERSION is the hash the modules were served under. A snapshot is a payload in
// the shape this build of the app reads, so one written by an older build is
// ignored rather than booted from: the shapes are not promised to match across
// a deploy, and a cached page is exactly where that would show.
const VERSION = import.meta.url.match(/\/static\/([^/]+)\//)?.[1] || 'dev';

// ponytail: the whole snapshot is written again two seconds after the last
// change rather than the rows that moved. At four hundred cards that is not
// what limits anything, measured: a replay of four hundred commands takes the
// time its pace asks for whether the snapshot is written through it or held
// until the end. Beyond a few thousand rows it would have to be one row at a
// time.
let keeping = 0;

export function remember() {
  if (!state.open || keeping || (state.fromCache && !booted)) return;
  keeping = setTimeout(write, 2000);
}

// booted says a page the worker handed back has read the snapshot and is
// drawing it. Until then it holds nothing worth keeping and writing would put
// an empty proposition over what this device has. After it, what is made on
// such a page is made nowhere else, so it is kept like anything else: without
// this a block drawn with no connection was gone at the next reload while the
// command that makes it was still in the outbox.
let booted = false;

function write() {
  clearTimeout(keeping);
  keeping = 0;
  if (!state.open || (state.fromCache && !booted)) return;
  offline.keep({
    proposition: state.open,
    at: Date.now(),
    v: VERSION,
    payload: snapshot(),
  });
}

// A tab closed inside the two seconds would otherwise leave the last few
// changes out of the snapshot, which is the reload that most wants them.
addEventListener('pagehide', () => { if (keeping) write(); });

// askTwice tells a read that could not be made from one that was made and found
// nothing. The database answers null for the first, which is what a tab holding
// the version before this one causes, and undefined for the second. A block
// lifts the moment that tab goes, and this page asks only once and then tells
// somebody their work is not on this device, so the first is worth one more go.
// The second is the answer, and asking again would only be slower.
// The second ask is a second chance rather than a second full wait: a block that
// has not lifted by now is one the boot should stop holding somebody up for,
// and the page it draws instead says plainly that it has nothing.
async function askTwice(read) {
  const first = await read();
  if (first !== null) return first;
  await new Promise((r) => setTimeout(r, 500));
  return Promise.race([read(), new Promise((r) => setTimeout(() => r(null), 1500))]);
}

// restore boots this page from what the last visit left behind. It answers
// false when nothing was cached for the proposition asked for, which is what
// the page says out loud rather than drawing an empty board.
export async function restore(open) {
  const row = await askTwice(() => offline.cached(open));
  if (!row || row.v !== VERSION) return false;
  boot(row.payload);
  state.open = open;
  // The links and files are a row of their own, written by the only thing that
  // reads them. A proposition whose panes were never opened has none, and the
  // panes say so rather than the board refusing to draw.
  const kept = await askTwice(() => offline.cachedMaterial(open));
  const material = kept && kept.v === VERSION ? kept : {};
  state.links = material.links || [];
  state.files = material.files || [];
  state.folders = material.folders || [];
  state.kinds = material.kinds || [];
  state.attachments = material.attachments || { links: [], files: [] };
  // The material is what was cached, so material() must not go looking for it.
  state.loaded = open;
  // From here this page holds what this device knows of the proposition, so
  // what is made on it is worth keeping.
  booted = true;
  // Only the proposition this device saw last can be changed offline. The rest
  // are read only from whatever was cached, which is what the plan asks for.
  readOnly = (await offline.newest()) !== open;
  return true;
}
