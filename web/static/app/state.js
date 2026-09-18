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
  return Boolean(state.can.edit && p && !p.archived_at);
}

export function archived() {
  const p = open();
  return Boolean(p && p.archived_at);
}

export function columnCards(columnID) {
  return [...state.cards.values()]
    .filter((c) => c.column_id === columnID)
    .sort(order);
}

const order = (a, b) => (a.position < b.position ? -1 : a.position > b.position ? 1 : 0);

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
        if (state.openCard === ev.entity_id) state.openCard = null;
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
    fields: (a, row) => ({ column_id: a.column, position: behind(a.after, row) }),
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

// behind is a position key that sorts just after the card the drop landed on.
// Keys are base 62, so a tilde is above every character one can end in.
//
// ponytail: it is a guess, not the key the server will allocate, and a card
// dropped above a neighbour whose key runs deeper than one character can land a
// place out until the echo arrives with the real one. The upgrade is the
// server's fractional key generator in the browser as well.
function behind(after, row) {
  if (!after) return '';
  const previous = state.cards.get(after);
  return previous ? previous.position + '~' : row.position;
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
    const now = rowOf(spec.entity, spec.id(args)) || was;
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
  if (!state.open || keeping || state.fromCache) return;
  keeping = setTimeout(write, 2000);
}

function write() {
  clearTimeout(keeping);
  keeping = 0;
  if (!state.open || state.fromCache) return;
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

// restore boots this page from what the last visit left behind. It answers
// false when nothing was cached for the proposition asked for, which is what
// the page says out loud rather than drawing an empty board.
export async function restore(open) {
  const row = await offline.cached(open);
  if (!row || row.v !== VERSION) return false;
  boot(row.payload);
  state.open = open;
  // The links and files are a row of their own, written by the only thing that
  // reads them. A proposition whose panes were never opened has none, and the
  // panes say so rather than the board refusing to draw.
  const kept = await offline.cachedMaterial(open);
  const material = kept && kept.v === VERSION ? kept : {};
  state.links = material.links || [];
  state.files = material.files || [];
  state.folders = material.folders || [];
  state.kinds = material.kinds || [];
  state.attachments = material.attachments || { links: [], files: [] };
  // The material is what was cached, so material() must not go looking for it.
  state.loaded = open;
  // Only the proposition this device saw last can be changed offline. The rest
  // are read only from whatever was cached, which is what the plan asks for.
  if ((await offline.newest()) !== open) state.can = { ...state.can, edit: false };
  return true;
}
