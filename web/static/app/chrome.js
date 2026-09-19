// What every page of the workspace has: the rail, the initials in the top bar,
// the palette, the offline line and the socket that keeps them live. The board
// and the per proposition settings page both start from here and add their own.

import { $, el, clear, initials, offlineLine, saying } from './dom.js';
import { state, boot, restore, subscribe, user } from './state.js';
import { connect, count } from './net.js';
import { renderRail } from './rail.js';
import { openPalette, closePalette } from './palette.js';
import { closePicker } from './picker.js';
import { closeDrawer } from './drawer.js';

export async function start(renderRest) {
  // A page the service worker handed back carries no payload, because it is the
  // one page here with nothing of anybody's in it. What it draws instead is the
  // snapshot this device kept of the proposition in the address bar.
  const payload = JSON.parse($('#payload').textContent);
  if (payload) {
    boot(payload);
  } else {
    state.fromCache = true;
    await restore(propositionInURL());
  }

  const render = () => {
    renderRail();
    renderPresence();
    renderNet();
    renderRest();
  };
  subscribe(render);
  render();
  connect();
  count();
  register();

  $('#top .search').addEventListener('click', openPalette);
  $('#railtoggle').addEventListener('click', () => document.body.classList.toggle('rail-open'));

  document.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      openPalette();
      return;
    }
    if (e.key !== 'Escape') return;
    if ($('#picker')) { closePicker(); return; }
    if (!$('#palette').hidden) { closePalette(); return; }
    if (state.openCard || state.openLink || state.openFile || state.panel) closeDrawer();
  });

  addEventListener('online', render);
  addEventListener('offline', render);
}

const propositionInURL = () => Number(location.pathname.match(/^\/p\/(\d+)$/)?.[1] || 0);

// register installs the service worker: the cached shell, the offline page and
// the assets. In development every asset is served under one unchanging path
// with no-store on it, so a worker holding copies would serve this morning's
// module through this afternoon's edit; there it is not installed at all.
function register() {
  if (!('serviceWorker' in navigator) || import.meta.url.includes('/static/dev/')) return;
  navigator.serviceWorker.register('/sw.js').catch(() => {
    // A browser that will not take one still runs the app; what it loses is the
    // page that appears when the server is unreachable.
  });
}

// The initials in the top bar are who else is on this proposition right now.
function renderPresence() {
  const bar = clear($('#top .presence'));
  for (const p of state.presence) bar.append(initials(user(p.id), 'on'));
  if (state.presence.length > 1) {
    bar.append(el('span', { class: 'mono', text: state.presence.length + ' here' }));
  }
  bar.title = state.presence.map((p) => user(p.id).name).join(', ');

  // The cached shell is rendered with no account and no workspace name on it,
  // so the top bar is filled in here from what the snapshot knew.
  if (!state.fromCache || !state.me) return;
  $('#top .show').textContent = state.workspace;
  const me = user(state.me);
  const acct = clear($('#top .acct'));
  acct.append(initials(me), el('span', { text: me.name.split(' ')[0] }));
}

// The offline line, as the plan draws it: a bar at the foot, nothing modal. It
// says the same thing when the browser has no network and when the outbox is
// still going up, because from the desk they are one situation.
function renderNet() {
  // A line somebody has just been told is worth more than the count, and it
  // puts itself away after four seconds.
  if (saying()) return;
  const bar = $('#netbar');
  const off = !navigator.onLine;
  if (!off && !state.waiting) {
    bar.hidden = true;
    return;
  }
  const held = state.waiting ? ` ${state.waiting} waiting.` : '';
  bar.textContent = off ? offlineLine + held : `Catching up.${held}`;
  bar.hidden = false;
}
