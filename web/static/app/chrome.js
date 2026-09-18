// What every page of the workspace has: the rail, the initials in the top bar,
// the palette, the offline line and the socket that keeps them live. The board
// and the per proposition settings page both start from here and add their own.

import { $, el, clear, initials, offlineLine } from './dom.js';
import { state, boot, subscribe, emit, user } from './state.js';
import { connect } from './net.js';
import { renderRail } from './rail.js';
import { openPalette, closePalette } from './palette.js';
import { closePicker } from './picker.js';
import { closeDrawer } from './drawer.js';

export function start(renderRest) {
  boot(JSON.parse($('#payload').textContent));

  const render = () => {
    renderRail();
    renderPresence();
    renderRest();
  };
  subscribe(render);
  render();
  connect();

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
    if (state.openCard) closeDrawer();
  });

  // The offline line. Nothing modal, as the plan asks.
  const netbar = $('#netbar');
  const network = () => {
    netbar.textContent = offlineLine;
    netbar.hidden = navigator.onLine;
  };
  addEventListener('online', network);
  addEventListener('offline', network);
  network();

  return emit;
}

// The initials in the top bar are who else is on this proposition right now.
function renderPresence() {
  const bar = clear($('#top .presence'));
  for (const p of state.presence) bar.append(initials(user(p.id), 'on'));
  if (state.presence.length > 1) {
    bar.append(el('span', { class: 'mono', text: state.presence.length + ' here' }));
  }
  bar.title = state.presence.map((p) => user(p.id).name).join(', ');
}
