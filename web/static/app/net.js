// The socket. One per tab, reconnecting on its own, and every command answered
// under the number this tab gave it so an optimistic change knows which answer
// is its own.
//
// Nothing here waits for the server before drawing. A command is applied to the
// local state, then sent if there is a socket and put in the outbox if there is
// not. The echo carries the whole row, so applying it after the guess is
// applying the guess again; a refusal puts back what was there and, for a
// command that was queued, leaves a row in the activity panel to choose from.

import { state, apply, emit, predict, baseText, target, retryMaterial } from './state.js';
import { parseWhere } from './blocktext.js';
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

// newKey names a change, as opposed to the request number above which names one
// attempt at sending it. The server remembers what it did under a name, so a
// command that has to go again, because this tab never saw the answer to the
// first, is answered with what the first one did rather than done twice.
//
// randomUUID is there in a secure context, which is every context this app runs
// in, including localhost. The fallback is for one served over plain http on
// another host, which the plan does not describe but a person reading the
// README might try.
export function newKey() {
  if (crypto.randomUUID) return crypto.randomUUID();
  return [...crypto.getRandomValues(new Uint8Array(16))]
    .map((b) => b.toString(16).padStart(2, '0')).join('');
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
    // This tab is where it was, and the server it is now talking to has never
    // been told: presence is a socket's own field, so without this the others
    // stop seeing this person until they move.
    tell();
    // A socket coming back is the moment a read that failed is worth making
    // again, so the links and files of the open proposition are marked unread
    // and the next render asks for them without waiting out the retry gap.
    retryMaterial();
    await catchUp();
    replay();
  });
  socket.addEventListener('message', (e) => receive(JSON.parse(e.data)));
  socket.addEventListener('close', () => {
    state.connected = false;
    // A command that was in flight when the socket went is not lost: it goes to
    // the back of the outbox and replays with the rest. The server may have
    // applied it before the socket died, and this tab has no way of knowing,
    // which is what the key on every command is for: the replay goes up under
    // the name the first attempt used, and a server that has already done it
    // answers with what it did rather than doing it again.
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
      // A replayed ack is the answer the server gave this command the first
      // time it arrived, which it is now saying again rather than applying
      // anything. Its payload is the row as it was at that moment and may be
      // older than what this tab holds now, so it is not drawn. Whatever this
      // tab actually missed is in the stream, and the stream is what it reads.
      if (m.event.replayed) {
        // Leaving it undrawn is an argument about the stream holding a newer
        // row, and the stream is one proposition's. A change filed under
        // another proposition, or under none, is carried by neither path:
        // catchUp reads the open proposition only, so a proposition started,
        // moved, archived or deleted from the rail would sit on the server and
        // be invisible in this tab until a reload. There is no newer row to
        // prefer over this one, so this one is drawn.
        if (m.event.proposition !== state.open) {
          apply(m.event);
        } else if (m.event.seq > state.seq) {
          // The row this answer is about has not reached this tab yet, and the
          // caller may be about to draw an editor on it. So the stream is read
          // first and the caller told after: told first, the render its answer
          // causes would find no block to put that editor in and would drop it,
          // taking the caret out of what somebody is typing into. A catchUp
          // that cannot reach the server leaves the block missing until the
          // next one, which is the same place a dropped event leaves it.
          catchUp().finally(() => { if (task) task.resolve(m.event); });
          break;
        }
      } else {
        apply(m.event);
      }
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
    case 'presence': {
      // Presence now arrives as often as anybody's caret moves, and a render
      // remakes the whole page. So the two are told apart: somebody arriving,
      // leaving or moving to another block changes what the top bar, the rail
      // and the margin say and is a render like any other, while a caret
      // moving inside the block it was already in is a redraw of that one
      // block.
      const people = m.people || [];
      const moved = places(people) !== places(state.presence);
      state.presence = people;
      if (moved || !carets) emit();
      else carets();
      break;
    }
    case 'gap':
      catchUp();
      break;
    case 'ping':
      // The server's heartbeat, answered from here rather than on a timer of
      // this tab's own: a hidden tab's timers are throttled to one a minute,
      // and a message handler runs when the frame arrives whatever the tab is
      // doing. A tab that is frozen answers nothing, is dropped, and reconnects
      // on the close event it gets when it wakes.
      if (socket && socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ cmd: 'pong' }));
      break;
  }
}

// carets is the light redraw a moving caret asks for, handed here by docs.js
// rather than imported from it: docs.js imports this module, and a module
// importing it back would work only for as long as neither top level happened
// to touch the other's exports, which is not something a page load tells you
// about twice. A page without it draws presence the way it always did.
let carets = null;

export function onCarets(fn) {
  carets = fn;
}

// places is who is where, with a caret inside a block reduced to the block:
// everything the page draws from presence other than the carets themselves is
// drawn from exactly this much. The list arrives in one order, by id, so two
// of these compare as strings.
const places = (people) => people.map((p) => {
  const at = parseWhere(p.where);
  return p.id + '@' + (at ? 'block:' + at.block : p.where || '');
}).join(',');

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
  const row = { proposition, me: state.me, cmd, args, idem: newKey(),
    base: args.base ?? null, base_text: baseWas };
  if (down() || replaying) return keep(row, target(cmd, args), revert);
  return ship(cmd, args, revert, row, row.idem);
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

function ship(cmd, args, revert, row, key) {
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
    socket.send(JSON.stringify({ id, cmd, key, args }));
  });
}

// live is a command that must not be kept for later. The editor's block.insert
// is the one: it carries text that is in no entry yet and has no row on the
// page, so the answer is what decides where that text ends up. Queued, it would
// arrive long after the editor had put the text back into the block it came
// from, and the paragraph would be there twice.
//
// A socket that goes while it is in the air used to be a refusal, and that was
// the same duplicate by another route: the server may have applied the insert
// before the socket died. So the frame carries a key and goes again, under the
// same key, as soon as there is a socket to go on. The server either does it or
// says it already did, and either way the answer is the one block.
//
// One deadline covers the whole attempt, tries and all, rather than one per
// try: the caller is holding text with nowhere to be, and what matters to them
// is how long until it lands somewhere, not how many times this tried. It gives
// up the way the drain does, for the same reason: a socket can be open and
// connected to nothing.
//
// It does not wait behind a replay the way send does. The outbox's order is
// about one field's edits folding into one another; an insert names a block the
// server already has and has nothing to queue behind.
export function live(cmd, args) {
  const key = newKey();
  return new Promise((resolve, reject) => {
    let over = false;
    const timer = setTimeout(() => {
      // The frame that was in the air when this fired may still be applied. The
      // caller has been told it was not, so it draws nothing; the block arrives
      // as an ordinary event like anybody else's.
      over = true;
      reject(new Offline());
    }, replyWait);
    const settle = (fn) => (v) => {
      if (over) return;
      over = true;
      clearTimeout(timer);
      fn(v);
    };
    const attempt = (first) => {
      if (over) return;
      if (down()) {
        // Nothing has gone up yet and there is no connection, so there is
        // nothing uncertain to wait out: this is the refusal it always was, and
        // the caller puts the text back now rather than in fifteen seconds.
        if (first) settle(reject)(new Offline());
        else setTimeout(() => attempt(false), tryAgainIn);
        return;
      }
      ship(cmd, args, null, null, key).then(settle(resolve), (err) => {
        if (over) return;
        // The socket went while the frame was in the air, which says nothing
        // about whether the server applied it. The key is what makes sending it
        // again safe either way.
        if (err instanceof Offline) {
          setTimeout(() => attempt(false), tryAgainIn);
          return;
        }
        settle(reject)(err);
      });
    };
    attempt(true);
  });
}

// tryAgainIn is how long live waits between tries. A reconnection starts at
// half a second and backs off, so this is short enough to take the first socket
// that opens and long enough not to spin while there is none.
const tryAgainIn = 250;

// where tells the others what this tab has open. It is never worth an answer,
// and the same string twice is nothing to tell: a caret moving inside a block
// says one of these every fifth of a second and most of them say what the last
// one said. What was meant is remembered whether or not it got away, because a
// socket that has just opened has been told nothing and tell() below says it
// again the moment there is somewhere to say it.
//
// A frame the server drops for going too fast is therefore never said again,
// which a tab keeping to the throttle in docs.js cannot provoke: three hundred
// a minute against an allowance of six hundred. A tab that has been tampered
// with to send faster is one whose own caret stops moving for the others.
let told = null;

function tell() {
  if (told !== null && socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify({ cmd: 'where', args: { where: told } }));
  }
}

export function where(what) {
  if (what === told) return;
  told = what;
  tell();
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
// refused for going too fast.
//
// Measured, four hundred commands go up in a hundred and two seconds, which is
// this wait and almost nothing else: the replay is paced rather than working
// hard. In a tab that is not the one being looked at it is four times slower,
// because a browser clamps a hidden tab's timers to one a second. That is the
// browser's decision and the right one; the queue is going up either way and
// nobody is watching it.
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
//
// tries counts what a row has been handed back for a reason that was nobody's
// decision. Three rounds of that and it stops being a hold-up and becomes an
// answer, so it goes to the panel with try it again beside let it go rather
// than sitting in the queue for ever in front of everything behind it.
//
// patience is how long the first of those rounds waits, and it doubles. Four
// seconds, eight, and then the row is handed back, so the three rounds span
// about twelve: long enough for a server to be restarted and come back, short
// enough that nobody is left wondering.
const rounds = 3;
const patience = 4000;

async function drain() {
  let pause = patience;
  const tries = new Map();
  for (;;) {
    const pass = (await offline.queued()).filter((row) => !row.refused);
    if (!pass.length) return;
    for (const stale of pass) {
      // The row is read again here rather than trusted from the pass: the
      // person may have typed into the same field since, and what goes up has
      // to be what they last wrote. A row filed before commands carried a key
      // is given one in the same read, so a second attempt at it reuses that
      // one and the server can tell the two attempts apart from two commands.
      const got = await offline.take(stale.n, newKey());
      const row = got && got.row;
      if (!row || row.refused) continue;
      // The outbox is this device's, but the person at it can change. Another
      // account's commands are not this one's to send.
      if (row.me && state.me && row.me !== state.me) {
        await offline.drop(row.n);
        continue;
      }
      try {
        await answered(row.via === 'api' ? post(row) : ship(row.cmd, row.args, null, null, row.idem),
          row.via === 'api' ? httpWait : replyWait);
        await offline.dropIfUnchanged(row.n, row.at);
        reverts.delete(row.n);
        tries.delete(row.n);
        pause = patience;
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
        const again = (tries.get(row.n) || 0) + 1;
        tries.set(row.n, again);
        if (transient(err) && again < rounds) {
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
// a link, which the server answers with the page it read. The row's name goes
// up as the header the API takes it under, so a request that timed out on the
// way back, and is sent again on the next pass, adds one link rather than two.
async function post(row) {
  if (row.cmd !== 'link.add') throw new Error('that did not go through');
  try {
    return await api.post('/links', row.args, { 'Idempotency-Key': row.idem });
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
// request goes in the outbox under its own name and is replayed as one. It
// answers the key it was filed under, or zero when there was no storage to file
// it in, because the pane promises to keep it and must not promise that when it
// could not.
export async function queueLink(proposition, url) {
  const n = await offline.queue({
    via: 'api', me: state.me, proposition, cmd: 'link.add', idem: newKey(),
    args: { proposition, url },
  });
  await count();
  return n;
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
//
// The read gives up after the same wait a command is given. Two things await
// this: the socket opening, which drains the outbox afterwards, and a replayed
// ack, which is holding a caller and its text. A connection that is open and
// answering nothing would hold either of them for the life of the page, which
// is the same thing answered guards every command against.
//
// Two of these can be in the air at once, one from each of those. Both ask from
// the same sequence number, so the later but shorter answer can draw an older
// row over a newer one, which the next event or reconnect puts right. Queueing
// them behind one another was worse: one read that never settled took every
// later one with it.
async function catchUp() {
  if (!state.open) return;
  try {
    const res = await fetch(`/api/events?proposition=${state.open}&since=${state.seq}&wait=0`, {
      headers: { Accept: 'application/json' },
      // Optional because a browser without it is one that waits as it did
      // before, which is the old behavior rather than a broken one.
      signal: AbortSignal.timeout?.(replyWait),
    });
    if (!res.ok) return;
    const body = await res.json();
    for (const ev of body.events || []) apply(ev);
  } catch {
    // Offline, or a read nobody answered in time. The next open tries again.
  }
}
