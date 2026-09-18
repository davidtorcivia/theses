// The state the page draws itself from. It starts as the payload the server
// rendered into the page and is moved forward by applied events, whether they
// came back from this tab's own command or arrived from somebody else's.

import * as api from './api.js';

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
  // when one went wrong, what to say about it.
  uploads: new Map(),
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
    emit();
  } catch (err) {
    state.loaded = 0;
    throw err;
  }
}

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
  emit();
}
