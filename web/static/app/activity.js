// The activity panel: what has happened on this proposition, newest first, with
// undo on the rows core allows it on, and above them the changes this device
// made offline that the server would not take.
//
// It lives in the drawer the cards and the links use, because it is the same
// thing: one column beside the work, closed with Escape.

import { el, initials, say } from './dom.js';
import { state, user, emit, canEdit } from './state.js';
import { send, again, letGo, resend } from './net.js';
import * as api from './api.js';

export function openPanel() {
  state.panel = true;
  state.openCard = state.openLink = state.openFile = null;
  seen = -1;
  emit();
}

export function closePanel() {
  state.panel = false;
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
  for (const row of state.activity) list.append(activityRow(row));
  drawer.append(list);
}

function activityRow(row) {
  const who = row.actor && row.actor.id ? user(row.actor.id) : { name: row.actor ? row.actor.name : '', initials: '··', colour: 'c8' };
  const line = el('div', {},
    el('p', { class: row.undone ? 'dim' : '', text: describe(row) }),
    el('span', { class: 'mono when', text: when(row.at) + (row.undone ? ' · undone' : '') }));
  if (row.undoable && canEdit()) {
    line.append(' ', el('button', {
      class: 'lnk quiet', type: 'button', text: 'undo',
      onclick: (e) => {
        e.currentTarget.disabled = true;
        send('undo', { activity: row.seq }).catch((err) => say(err.message));
      },
    }));
  }
  return el('li', {}, initials(who), line);
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
