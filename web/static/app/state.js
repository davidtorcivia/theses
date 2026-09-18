// The state the page draws itself from. It starts as the payload the server
// rendered into the page and is moved forward by applied events, whether they
// came back from this tab's own command or arrived from somebody else's.

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

  tab: 'board',
  railFilter: null,
  boardFilter: 'all',
  openCard: null,
  connected: false,
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
}

function loadBoard(board) {
  state.columns = board ? board.columns || [] : [];
  state.cards = new Map();
  for (const c of (board ? board.cards || [] : [])) state.cards.set(c.id, c);
  state.seq = board ? board.seq || 0 : 0;
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
