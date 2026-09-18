// The socket. One per tab, reconnecting on its own, and every command answered
// under the number this tab gave it so an optimistic change knows which answer
// is its own.

import { state, apply, emit } from './state.js';

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
  socket.addEventListener('open', () => {
    backoff = 500;
    state.connected = true;
    catchUp();
    emit();
  });
  socket.addEventListener('message', (e) => receive(JSON.parse(e.data)));
  socket.addEventListener('close', () => {
    state.connected = false;
    for (const [id, task] of waiting) { task.reject(new Offline()); waiting.delete(id); }
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
      if (task) task.reject(new Conflict(m.conflict));
      break;
    case 'error':
      if (task) task.reject(new Error(m.error));
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

// send resolves with the applied event, or rejects with a Conflict the editor
// can offer a choice about, or with an Offline the caller reverts on.
export function send(cmd, args = {}) {
  return new Promise((resolve, reject) => {
    if (!socket || socket.readyState !== WebSocket.OPEN) { reject(new Offline()); return; }
    const id = next++;
    waiting.set(id, { resolve, reject });
    socket.send(JSON.stringify({ id, cmd, args }));
  });
}

// where tells the others what this tab has open. It is never worth an answer.
export function where(what) {
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify({ cmd: 'where', args: { where: what } }));
  }
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
