// The socket. One per tab, reconnecting on its own, and every command answered
// under the number this tab gave it so an optimistic change knows which answer
// is its own.
//
// Nothing here waits for the server before drawing. A command is applied to the
// local state, then sent if there is a socket and put in the outbox if there is
// not. The echo carries the whole row, so applying it after the guess is
// applying the guess again; a refusal puts back what was there and, for a
// command that was queued, leaves a row in the activity panel to choose from.

import { state, apply, emit, predict, baseText, target } from './state.js';
import * as offline from './offline.js';
import * as api from './api.js';

let socket = null;
let next = 1;
let backoff = 500;
const waiting = new Map();

// reverts holds the function that puts a queued command's guess back, by the
// outbox key it was filed under. A command that never went up has been drawn
// here and nowhere else, so a refusal, or a person choosing to let it go, has to
// undraw it. The map does not survive a reload, and does not need to: a page
// that reloads draws itself from the server again.
const reverts = new Map();

function undraw(n, detail) {
  const back = reverts.get(n);
  reverts.delete(n);
  if (back) back(detail);
}

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
      // A command this tab made goes to the back of the outbox and replays with
      // the rest. A command that was already being replayed out of the outbox is
      // still in it, so it is refused here and the drain leaves it where it is.
      if (task.row) { task.queue(); task.resolve(null); } else { task.reject(new Offline()); }
    }
    emit();
    stillSignedIn();
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
export function send(cmd, args = {}, proposition = state.open) {
  const baseWas = baseText(cmd, args);
  const revert = predict(cmd, args);
  const row = { proposition, me: state.me, cmd, args, base: args.base ?? null, base_text: baseWas };
  if (down() || replaying) return keep(row, target(cmd, args), revert);
  return ship(cmd, args, revert, row);
}

// keep puts a command in the outbox and answers as though it had gone. It has
// been applied here; the panel is where it turns up again if the server will
// not take it. A browser with no storage to keep it in cannot promise that, so
// there the guess is undrawn and the caller is told, as it was before any of
// this existed.
async function keep(row, key, revert) {
  const n = await offline.queue(row, key);
  if (!n) {
    if (revert) revert();
    await count();
    throw new Offline();
  }
  // The oldest guess is the one to put back: two edits folded into one entry
  // undraw all the way to what the server still has.
  if (revert && !reverts.has(n)) reverts.set(n, revert);
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
      row,
      revert: revert || (() => {}),
      queue: () => { if (row) offline.queue(row, target(cmd, args)).then(count); },
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

// pace is how long the drain waits between commands. The socket takes three
// hundred a minute from one tab, which is five a second, and a queue that has
// been filling all afternoon would otherwise spend its first second being
// refused for going too fast. Four a second stays under it.
const pace = 250;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// transient is a refusal that says nothing about the command. Going too fast,
// a session the server has not finished with, and a fault on its side are all
// worth trying again; everything else is the server's answer and stands.
function transient(err) {
  if (err instanceof Conflict) return false;
  if (err.status >= 500 || err.status === 401) return true;
  return /too many changes|wait a moment/i.test(err.message || '');
}

// drain takes pass after pass, because a change made while the queue is going
// up joins the back of it: one pass over a list read at the start would leave
// it sitting there until the next reconnection. Every pass drops, marks or
// backs off on every row it sees, so there is always one less to do.
async function drain() {
  let pause = 500;
  for (let gaveUp = 0; gaveUp < 8; ) {
    const rows = (await offline.queued()).filter((row) => !row.refused);
    if (!rows.length) return;
    for (const row of rows) {
      // The outbox is this device's, but the person at it can change. Another
      // account's commands are not this one's to send.
      if (row.me && state.me && row.me !== state.me) {
        await offline.drop(row.n);
        continue;
      }
      try {
        await answered(row.via === 'api' ? post(row) : ship(row.cmd, row.args, null, null),
          row.via === 'api' ? httpWait : replyWait);
        await offline.dropIfUnchanged(row.n, row.at);
        reverts.delete(row.n);
        pause = 500;
        await sleep(pace);
      } catch (err) {
        if (err instanceof Offline) {
          // Either the socket has gone or it is open and answering nothing,
          // which looks the same from here. Closing it is what starts the
          // reconnection that comes back and finishes the queue. A request that
          // timed out says nothing about the socket, so it is left alone.
          if (socket && row.via !== 'api') socket.close();
          return;
        }
        if (transient(err)) {
          gaveUp++;
          await sleep(pause);
          pause = Math.min(pause * 2, 8000);
          break;
        }
        const detail = err instanceof Conflict ? err.detail : null;
        await offline.refuse(row.n, row.at, err.message, detail);
        undraw(row.n, detail);
        await sleep(pace);
      }
    }
  }
  // Eight goes and the server is still saying not now. The queue keeps its
  // place and another attempt is made shortly; nothing is lost either way.
  setTimeout(replay, 15000);
}

// answered gives up on a command the server never answers. A socket can be open
// and connected to nothing at all, which is what a laptop that has changed
// network has, and without this one such command would hold the queue shut for
// ever: nothing else may go past it and nothing would ever unstick it.
//
// A queued link is the slow one: the server fetches the page before it answers,
// with its own timeouts, and giving up early would send the same link twice.
const replyWait = 15000;
const httpWait = 40000;

function answered(promise, wait) {
  let timer = 0;
  return Promise.race([
    promise.finally(() => clearTimeout(timer)),
    new Promise((_, no) => { timer = setTimeout(() => no(new Offline()), wait); }),
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
// stocked with the replays the server would not take. The bar counts the whole
// device, because that is what it promises to keep; the panel and the tab
// beside it are one proposition's, because that is what they are about.
export async function count() {
  const rows = (await offline.queued()).filter((r) => !r.me || !state.me || r.me === state.me);
  const mine = rows.filter((r) => r.proposition === state.open);
  state.waiting = rows.filter((r) => !r.refused).length;
  state.waitingHere = mine.filter((r) => !r.refused).length;
  state.refused = mine.filter((r) => r.refused);
  emit();
}

// retry puts a refused row back in the queue, which is what the panel offers on
// a refusal that was nobody's decision.
export async function again(n) {
  await offline.retry(n);
  await count();
  replay();
}

// letGo drops a refused row and undraws the guess that was never taken, to what
// the server says it holds when it said, and to what was there before when it
// did not.
export async function letGo(n, detail) {
  await offline.drop(n);
  undraw(n, detail);
  await count();
}

// resend is keep mine: the command goes again with the version that is there
// now. The new send has drawn the new value itself, so the old guess is dropped
// without being undrawn; undrawing it would take the new one away with it.
export async function resend(row, args) {
  reverts.delete(row.n);
  await offline.drop(row.n);
  await count();
  return send(row.cmd, args, row.proposition);
}

// queueLink is the links pane's way in: no socket command adds a link, so the
// request goes in the outbox under its own name and is replayed as one.
export async function queueLink(proposition, url) {
  await offline.queue({ via: 'api', me: state.me, proposition, cmd: 'link.add', args: { proposition, url } });
  await count();
}

// stillSignedIn tells a server that is down from a session that has ended. Both
// look like a socket that will not open, and they want opposite things: one is
// waited out, the other has to take this device's copy of the workspace with
// it. The answer comes back through api.js, which calls signedOut itself on a
// session the server no longer knows.
function stillSignedIn() {
  if (!navigator.onLine) return;
  api.get('/activity?proposition=' + state.open).catch(() => {});
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
