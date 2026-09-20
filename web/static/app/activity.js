// The activity panel: what has happened on this proposition, newest first, with
// undo on the rows core allows it on, and above them the changes this device
// made offline that the server would not take.
//
// It lives in the drawer the cards and the links use, because it is the same
// thing: one column beside the work, closed with Escape.

import { el, initials, say } from './dom.js';
import { state, user, emit, canEdit } from './state.js';
import { send, again, letGo, resend, Conflict } from './net.js';
import * as api from './api.js';

export function openPanel() {
  state.panel = true;
  state.openCard = state.openLink = state.openFile = null;
  seen = -1;
  emit();
}

export function closePanel() {
  state.panel = false;
  // The settings page's Activity tab links to this hash. Left on the address
  // after the panel has been closed, pressing that tab again is a link to the
  // page it is already on and nothing happens at all.
  if (location.hash === '#activity') {
    history.replaceState(null, '', location.pathname + location.search);
  }
  emit();
}

// seen is the stream position the list was read at. Every applied event moves
// the state on, so the panel notices it is behind and reads again, which is
// also how an undo row appears without this having to guess what undo did.
let seen = -1;
let timer = 0;

function refresh() {
  if (seen === state.seq || timer || !state.open) return;
  timer = setTimeout(async () => {
    timer = 0;
    const at = state.seq;
    try {
      const body = await api.get('/activity?proposition=' + state.open);
      seen = at;
      state.activity = body.activity || [];
      emit();
    } catch {
      // Offline, or a proposition this person may no longer read. The refused
      // rows above are the part that matters with no server, and they are
      // already here.
    }
  }, 300);
}

export function renderPanel(drawer) {
  refresh();
  drawer.append(el('div', { class: 'dh' },
    el('span', { class: 'mono', text: 'Activity' }),
    el('button', { class: 'x', type: 'button', text: 'Close', onclick: closePanel })));

  if (state.refused.length) {
    drawer.append(el('h4', { text: 'Not taken' }));
    const list = el('ul', { class: 'linked' });
    for (const row of state.refused) list.append(refusedRow(row));
    drawer.append(list);
  }

  drawer.append(el('h4', { text: 'Recent' }));
  const list = el('ol', { class: 'comments' });
  if (!state.activity.length) {
    list.append(el('li', { class: 'dim', text: state.fromCache
      ? 'The log is read from the server. It is here when you are back online.'
      : 'Nothing yet.' }));
  }
  for (const group of grouped(state.activity)) list.append(activityRow(group));
  drawer.append(list);
}

// grouped folds a run of saves by one person on one block into a single line.
// A document saves itself as it is typed, so every block someone writes leaves
// a row a second, and listing all of them would make the panel a typing log
// with the rest of the work scrolled off the bottom of it.
function grouped(rows) {
  const out = [];
  for (const row of rows) {
    const last = out[out.length - 1];
    if (last && follows(last[0], row)) last.push(row);
    else out.push([row]);
  }
  return out;
}

// sitting is how far apart two saves may be and still be the same run. A
// document saves itself every few hundred milliseconds while somebody types, so
// a gap of minutes is them coming back to the block rather than still being in
// it, and folding the two together would put one line on the panel for an
// afternoon and offer to take the whole afternoon back.
const sitting = 120;

const follows = (a, b) => a.entity === 'block' && a.action === 'set'
  && b.entity === 'block' && b.action === 'set' && a.entity_id === b.entity_id
  && Boolean(a.actor) && Boolean(b.actor)
  && a.actor.kind === b.actor.kind && a.actor.id === b.actor.id
  && a.at - b.at <= sitting;

// A group is drawn at the newest of its rows, which is where its time comes
// from and what its text says.
function activityRow(group) {
  const row = group[0];
  const who = row.actor && row.actor.id ? user(row.actor.id) : { name: row.actor ? row.actor.name : '', initials: '··', colour: 'c8' };
  const line = el('div', {},
    el('p', { class: row.undone ? 'dim' : '', text: describe(row) }),
    el('span', { class: 'mono when', text: when(row.at)
      + (group.length > 1 ? ` · ${group.length} saves` : '')
      + (row.undone ? ' · undone' : '') }));
  const back = canEdit() ? (group.length > 1 ? takeRunBack(group) : takeRowBack(row)) : null;
  if (back) line.append(' ', back);
  return el('li', {}, initials(who), line);
}

// takeRowBack is core's undo of one row: its before put back, and the row
// marked undone so nobody puts it back twice.
function takeRowBack(row) {
  if (!row.undoable) return null;
  return el('button', {
    class: 'lnk quiet', type: 'button', text: 'undo',
    onclick: (e) => {
      e.currentTarget.disabled = true;
      send('undo', { activity: row.seq }).catch((err) => say(err.message));
    },
  });
}

// takeRunBack is the same offer over a run of saves, which core's undo cannot
// make: that one puts one row's before back and refuses a row the entity has
// moved past since, so the only row of a run it would take is the newest, which
// is a second of typing rather than the change the line describes.
//
// A run is taken back as an ordinary edit instead: the text the block held
// before the oldest row, sent with the version it reached after the newest. The
// server merges a set whose base is an older version like any other, so
// whatever anybody did to another part of the block after the run is kept and
// only this run's own words go. Nothing here is marked undone, because nothing
// was undone: this is a new change that happens to restore old text, and the
// log says exactly that, which is also what makes it undoable in its turn.
//
// Nothing is offered when the oldest row has no before to go back to, when the
// run left the block reading what it read before it, or when any row of it has
// already been undone on its own: the block then holds the text from before
// that row, and a set based on the version after it is a revert against a
// revert, which is the overlap no merge can make honestly.
function takeRunBack(group) {
  const last = group[0];
  const first = group[group.length - 1];
  const was = first.before && first.before.text;
  const now = last.after && last.after.text;
  if (typeof was !== 'string' || !last.after || !last.after.version) return null;
  if (was === now || group.some((r) => r.undone)) return null;
  return el('button', {
    class: 'lnk quiet', type: 'button', text: 'undo',
    onclick: (e) => {
      e.currentTarget.disabled = true;
      // whole, because that text was stored exactly as it was typed once
      // already, edges and blank lines included, and putting it back is putting
      // back what was there rather than writing something new.
      send('block.set', { block: last.entity_id, base: last.after.version, text: was, whole: true })
        .catch((err) => say(err instanceof Conflict
          ? 'That block has changed too much since for those saves to be taken back.'
          : err.message));
    },
  });
}

// describe is one line of English for one applied command. The log holds the
// row either side of the change, so the name of the thing is in the payload
// rather than in a table of phrasings here.
function describe(row) {
  const subject = row.after || row.before || {};
  const name = subject.title || subject.name || subject.text || subject.url || '';
  const what = `${row.action} ${row.entity.replace(/_/g, ' ')}`;
  return name ? `${what} · ${clip(name)}` : what;
}

const clip = (text) => (text.length > 80 ? text.slice(0, 79) + '…' : text);

// refusedRow is one command the server would not take when it went up. It shows
// the three texts a merge is about, which is why the outbox keeps the one the
// editor started from: without it a choice made an hour later is made blind.
function refusedRow(row) {
  const li = el('li', {});
  const detail = row.detail;
  const mine = row.args.text ?? row.args.title ?? '';
  li.append(el('p', { text: `${row.cmd} was not taken: ${row.refused}` }));
  if (row.base_text) {
    li.append(el('p', { class: 'mono dim', text: 'You started from: ' + clip(row.base_text) }));
  }
  if (mine) li.append(el('p', { class: 'mono dim', text: 'Yours: ' + clip(String(mine)) }));
  if (detail) {
    li.append(el('p', { class: 'mono dim', text: 'Theirs: ' + clip(detail.current || '(nothing)') }));
    li.append(el('button', {
      class: 'lnk', type: 'button', text: 'Keep mine',
      onclick: () => resolve(row, { ...row.args, base: detail.version }),
    }), ' ');
  } else {
    // A refusal with nothing to compare is the server saying no rather than
    // somebody else saying something different, and some of those are worth one
    // more go: a moment when it was busy, a fault it has since recovered from.
    li.append(el('button', { class: 'lnk', type: 'button', text: 'Try it again', onclick: () => again(row.n) }), ' ');
  }
  li.append(el('button', {
    class: 'lnk plain', type: 'button', text: detail ? 'Take theirs' : 'Let it go',
    onclick: () => resolve(row, null),
  }));
  return li;
}

// resolve is what the choice comes to: send it again as it now has to be sent,
// or drop it and put the row back to what the server says it holds. Either way
// the entry leaves the outbox, because the choice has been made and offering it
// a second time would be asking twice. The row carries the proposition it was
// made on, which need not be the open one.
async function resolve(row, args) {
  if (!args) {
    await letGo(row.n, row.detail);
    return;
  }
  try {
    await resend(row, args);
  } catch (err) {
    say(err.message);
  }
}

function when(unix) {
  const seconds = Math.max(0, Math.floor(Date.now() / 1000) - unix);
  if (seconds < 60) return 'just now';
  if (seconds < 3600) return Math.floor(seconds / 60) + ' min ago';
  if (seconds < 86400) return Math.floor(seconds / 3600) + ' h ago';
  return new Date(unix * 1000).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

// outstanding is what the tab says beside its name: the queue plus whatever the
// server has already refused.
export const outstanding = () => state.waitingHere + state.refused.length;
