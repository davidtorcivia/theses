// The socket. One per tab, reconnecting on its own, and every command answered
// under the number this tab gave it so an optimistic change knows which answer
// is its own.
//
// Nothing here waits for the server before drawing. A command is applied to the
// local state, then sent if there is a socket and put in the outbox if there is
// not. The echo carries the whole row, so applying it after the guess is
// applying the guess again; a refusal puts back what was there and, for a
// command that was queued, leaves a row in the activity panel to choose from.

import { state, apply, emit, predict, baseText } from './state.js';
import * as offline from './offline.js';
import * as api from './api.js';

let socket = null;
let next = 1;
let backoff = 500;
const waiting = new Map();

export class Offline extends Error {
  constructor() {
    super('You are offline. That change was not saved.');
  }
}

export class Conflict extends Error {
  constructor(detail) {
    super('Somebody changed that while you were editing it.');
    this.detail = detail;
  }
}

export function connect() {
  // A workspace with nothing in it still opens a socket, because creating the
  // first proposition goes through it.
  const url = new URL('/ws', location.href);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  url.searchParams.set('proposition', state.open);

  socket = new WebSocket(url);
  socket.addEventListener('open', async () => {
    backoff = 500;
    state.connected = true;
    emit();
    // A page the service worker handed over has no payload and no CSRF token in
    // it, so it cannot post anything. Now that there is a server again the real
    // page is one reload away, and the outbox is on disk and survives it.
    if (state.fromCache) {
      location.reload();
      return;
    }
    await catchUp();
    replay();
  });
  socket.addEventListener('message', (e) => receive(JSON.parse(e.data)));
  socket.addEventListener('close', () => {
    state.connected = false;
    // A command that was in flight when the socket went is not lost: it goes to
    // the back of the outbox and replays with the rest.
    //
    // ponytail: the server may have applied it before the socket died, so a
    // create can arrive twice. Everything else is a field being set to a value
    // it already holds. The upgrade is a key the client picks and the server
    // remembers, which is a schema change.
    for (const [id, task] of waiting) {
      waiting.delete(id);
      task.queue();
      task.resolve(null);
    }
    emit();
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, 15000);
  });
}

function receive(m) {
  const task = m.id ? waiting.get(m.id) : null;
  if (task) waiting.delete(m.id);
  switch (m.type) {
    case 'ack':
      apply(m.event);
      if (task) task.resolve(m.event);
      break;
    case 'conflict':
      if (task) { task.revert(); task.reject(new Conflict(m.conflict)); }
      break;
    case 'error':
      if (task) { task.revert(); task.reject(new Error(m.error)); }
      break;
    case 'event':
      apply(m.event);
      break;
    case 'presence':
      state.presence = m.people || [];
      emit();
      break;
    case 'gap':
      catchUp();
      break;
  }
}

// down is every reason a command cannot go now. The browser's own flag is in it
// as well as the socket's state, because a socket that has not noticed the
// network is gone yet would swallow the command rather than queue it.
function down() {
  return !navigator.onLine || !socket || socket.readyState !== WebSocket.OPEN;
}

// send applies the command here, then either sends it or keeps it. It resolves
// with the applied event, with null for a command that was queued, and rejects
// with a Conflict or a refusal the caller can offer a choice about.
export function send(cmd, args = {}) {
  const baseWas = baseText(cmd, args);
  const revert = predict(cmd, args);
  const row = { proposition: state.open, cmd, args, base: args.base ?? null, base_text: baseWas };
  if (down() || replaying) return keep(row);
  return ship(cmd, args, revert, row);
}

// keep puts a command in the outbox and answers as though it had gone. It has
// been applied here; the panel is where it turns up again if the server will
// not take it.
async function keep(row) {
  await offline.queue(row);
  await count();
  return null;
}

function ship(cmd, args, revert, row) {
  return new Promise((resolve, reject) => {
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      if (revert) revert();
      reject(new Offline());
      return;
    }
    const id = next++;
    waiting.set(id, {
      resolve,
      reject,
      revert: revert || (() => {}),
      queue: () => { if (row) offline.queue(row).then(count); },
    });
    socket.send(JSON.stringify({ id, cmd, args }));
  });
}

// where tells the others what this tab has open. It is never worth an answer.
export function where(what) {
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify({ cmd: 'where', args: { where: what } }));
  }
}

// replay empties the outbox in the order it was filled, one at a time, through
// the same path a live command takes: the server's merge is what settles a set
// that went stale while this device was away. An entry leaves the outbox on any
// answer at all; only a socket that goes again puts the rest back to waiting.
let replaying = false;

export async function replay() {
  if (replaying) return;
  replaying = true;
  try {
    // The outbox belongs to the device, not to this tab, so any tab with a
    // socket drains it. Two of them draining it at once would send the same
    // command twice, and the browser's own lock is what stops that.
    await (navigator.locks ? navigator.locks.request('theses-outbox', drain) : drain());
  } finally {
    replaying = false;
    await count();
  }
}

// drain takes pass after pass, because a change made while the queue is going
// up joins the back of it: one pass over a list read at the start would leave
// it sitting there until the next reconnection. Every pass drops or marks every
// row it sees, so there is always one less to do.
async function drain() {
  for (;;) {
    const rows = (await offline.queued()).filter((row) => !row.refused);
    if (!rows.length) return;
    for (const row of rows) {
      try {
        await answered(row.via === 'api' ? post(row) : ship(row.cmd, row.args, null, null));
        await offline.drop(row.n);
      } catch (err) {
        if (err instanceof Offline) {
          // Either the socket has gone or it is open and answering nothing,
          // which looks the same from here. Closing it is what starts the
          // reconnection that comes back and finishes the queue.
          if (socket) socket.close();
          return;
        }
        await offline.refuse(row.n, err.message, err instanceof Conflict ? err.detail : null);
      }
    }
  }
}

// answered gives up on a command the server never answers. A socket can be open
// and connected to nothing at all, which is what a laptop that has changed
// network has, and without this one such command would hold the queue shut for
// ever: nothing else may go past it and nothing would ever unstick it.
const replyWait = 15000;

function answered(promise) {
  let timer = 0;
  return Promise.race([
    promise.finally(() => clearTimeout(timer)),
    new Promise((_, no) => { timer = setTimeout(() => no(new Offline()), replyWait); }),
  ]);
}

// A browser noticing a network is the other way a replay starts. A socket that
// was never actually broken, which is what a machine coming back from sleep
// often has, fires no close and therefore no open.
addEventListener('online', () => {
  if (socket && socket.readyState === WebSocket.OPEN) replay();
});

// post is a queued command that is a request rather than a socket frame: adding
// a link, which the server answers with the page it read.
async function post(row) {
  if (row.cmd !== 'link.add') throw new Error('that did not go through');
  try {
    return await api.post('/links', row.args);
  } catch (err) {
    if (err.status === 0) throw new Offline();
    throw err;
  }
}

// count keeps the offline bar honest about how much is waiting, and the panel
// stocked with the replays the server would not take.
export async function count() {
  const rows = await offline.queued();
  state.waiting = rows.filter((r) => !r.refused).length;
  state.refused = rows.filter((r) => r.refused);
  emit();
}

// queueLink is the links pane's way in: no socket command adds a link, so the
// request goes in the outbox under its own name and is replayed as one.
export async function queueLink(proposition, url) {
  await offline.queue({ via: 'api', proposition, cmd: 'link.add', args: { proposition, url } });
  await count();
}

// catchUp reads whatever this tab missed out of the activity table, which is
// what a reconnect and a dropped event both need.
async function catchUp() {
  if (!state.open) return;
  try {
    const res = await fetch(`/api/events?proposition=${state.open}&since=${state.seq}&wait=0`, {
      headers: { Accept: 'application/json' },
    });
    if (!res.ok) return;
    const body = await res.json();
    for (const ev of body.events || []) apply(ev);
  } catch {
    // Offline. The next open will try again.
  }
}
