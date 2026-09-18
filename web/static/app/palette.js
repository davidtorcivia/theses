// The palette over propositions and cards. It searches what this tab already
// holds, which at four people and thirty propositions is everything.

import { $, el, clear, num } from './dom.js';
import { state } from './state.js';
import { openCard } from './drawer.js';

export function openPalette() {
  const palette = $('#palette');
  palette.hidden = false;
  const field = palette.querySelector('input');
  field.value = '';
  field.focus();
  list('');
}

export function closePalette() {
  $('#palette').hidden = true;
}

function items() {
  const rows = state.props.map((p) => ({
    kind: 'proposition', label: num(p.number) + ' ' + p.title, go: () => { location.href = '/p/' + p.id; },
  }));
  for (const card of state.cards.values()) {
    // On a page with no drawer, a card is a reason to go back to the board.
    rows.push({ kind: 'card', label: card.title, go: () => {
      if (!$('#drawer')) { location.href = '/p/' + state.open; return; }
      closePalette();
      openCard(card.id);
    } });
  }
  if (state.can.settings) {
    rows.push({ kind: 'go', label: 'Workspace settings', go: () => { location.href = '/settings'; } });
  }
  if (state.open) {
    rows.push({ kind: 'go', label: 'This proposition’s settings', go: () => { location.href = `/p/${state.open}/settings`; } });
  }
  rows.push({ kind: 'go', label: 'Your profile', go: () => { location.href = '/profile'; } });
  return rows;
}

function list(query) {
  const q = query.toLowerCase();
  const results = clear($('#palette ul'));
  const hits = items().filter((r) => !q || r.label.toLowerCase().includes(q)).slice(0, 12);
  if (!hits.length) {
    results.append(el('li', { class: 'none', text: 'No matches.' }));
    return;
  }
  for (const hit of hits) {
    results.append(el('li', {}, el('a', {
      href: '#', onclick: (e) => { e.preventDefault(); hit.go(); },
    }, el('span', { class: 'k mono', text: hit.kind }), hit.label)));
  }
}

$('#palette input').addEventListener('input', (e) => list(e.target.value));
$('#palette').addEventListener('click', (e) => {
  if (e.target.id === 'palette') closePalette();
});
$('#palette input').addEventListener('keydown', (e) => {
  if (e.key !== 'Enter') return;
  const first = $('#palette ul a');
  if (first) first.click();
});
