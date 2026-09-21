// The palette. It asks the server as you type, which is the one search in this
// app that reads everything: cards, document blocks, links, files, comments,
// propositions and people, grouped and with a line of the matching text. The
// propositions this tab already holds and the handful of things the palette
// does are listed from here, so it is never empty and never waits.

import { $, $$, el, clear, num } from './dom.js';
import { state, emit, material } from './state.js';
import { openCard } from './drawer.js';
import { openLink } from './links.js';
import { openFile } from './files.js';
import * as api from './api.js';
import { redrawFocus } from './palettefocus.js';

// How long a key press waits for the next one. Long enough that typing a word
// is one query rather than five, short enough that the list is there by the
// time the eye is.
const pause = 150;

// Per kind. Seven kinds of ten is a page of results; a palette shows a handful.
const perKind = 5;

// The request being drawn. A reply that is not the newest is thrown away,
// because two queries in flight come back in whatever order they like.
let asked = 0;
let timer = 0;
let returnTo = null;

export function openPalette() {
  // A reply to whatever was typed the last time it was open belongs to nobody.
  clearTimeout(timer);
  asked++;
  const palette = $('#palette');
  if (palette.hidden) returnTo = document.activeElement;
  palette.hidden = false;
  const field = palette.querySelector('input');
  field.value = '';
  field.focus();
  list('', []);
}

export function closePalette() {
  clearTimeout(timer);
  asked++;
  $('#palette').hidden = true;
  const back = returnTo;
  returnTo = null;
  if (back && back.isConnected) back.focus();
}

// The rows this tab can offer without asking anybody: the propositions it knows
// of, and what the palette does.
function local() {
  const rows = state.props.map((p) => ({
    kind: p.kind === 'show' ? 'show' : 'proposition',
    label: p.kind === 'show' ? 'Show' : num(p.number) + ' ' + p.title,
    go: () => { location.href = pathFor(p); },
  }));
  if (state.can.edit) {
    rows.push({ kind: 'go', label: 'New proposition', go: newProposition });
  }
  if (state.can.settings) {
    rows.push({ kind: 'go', label: 'Workspace settings', go: () => { location.href = '/settings'; } });
  }
  if (state.open) {
    const p = state.props.find((item) => item.id === state.open);
    rows.push({
      kind: 'go', label: p?.kind === 'show' ? 'Show settings' : 'This proposition’s settings',
      go: () => { location.href = pathFor(p, true); },
    });
  }
  rows.push({ kind: 'go', label: 'Your profile', go: () => { location.href = '/profile'; } });
  return rows;
}

function pathFor(p, settings = false, id = 0) {
  if (!p && !id) return '/';
  const path = p?.kind === 'show' ? '/show' : '/p/' + (p?.id || id);
  return path + (settings ? '/settings' : '');
}

// The rail's own form is where a proposition is made, so the palette opens that
// rather than owning a second way to make one. The rail is off the side of a
// phone until it is asked for, and a form nobody can see is not an answer, so
// it is opened first. A page with no rail on it goes to the board, which has
// one.
function newProposition() {
  closePalette();
  const button = $('#newprop');
  if (!button) { location.href = '/'; return; }
  if (matchMedia('(max-width: 900px)').matches && !document.body.classList.contains('rail-open')) {
    $('#railtoggle')?.click();
  }
  button.click();
}

// remote turns a hit into a row. Everything but a proposition and a person
// belongs to one proposition, and reaching it from another proposition's page
// is a navigation rather than a drawer.
function remote(hit) {
  const row = { kind: hit.kind, label: hit.title, snippet: aside(hit), go: null };
  const proposition = state.props.find((p) => p.id === hit.proposition_id);
  // A drawer to open it in is the other half of being on the right page: the
  // per proposition settings page carries the palette and none of the panes.
  const here = hit.proposition_id === state.open && Boolean($('#drawer'));
  if (hit.kind === 'proposition') row.go = () => {
    location.href = pathFor(state.props.find((p) => p.id === hit.id), false, hit.id);
  };
  // A person is somebody to know is here. The Team table is the only page about
  // them and only the owner may open it, so for everybody else the row says
  // what it found and goes nowhere.
  else if (hit.kind === 'user') row.go = state.can.settings ? () => { location.href = '/settings'; } : null;
  else if (hit.kind === 'block' && proposition?.kind === 'show' && !here) {
    row.go = () => { location.href = '/show#notes'; };
  }
  else if (!here) row.go = () => { location.href = pathFor(proposition, false, hit.proposition_id); };
  // A card made since this page was rendered is not in this tab's board, and
  // the drawer clears what it cannot find, so the proposition is read again.
  else if (hit.kind === 'card') row.go = () => {
    closePalette();
    if (state.cards.has(hit.id)) openCard(hit.id);
    else location.href = pathFor(proposition, false, hit.proposition_id);
  };
  // The links and the files are two requests away and this tab may not have
  // been on either pane. Opening the drawer before they are here is the drawer
  // finding nothing and putting itself away again, so it waits for them.
  else if (hit.kind === 'link') row.go = () => pane('links', () => openLink(hit.id));
  else if (hit.kind === 'file') row.go = () => pane('files', () => openFile(hit.id));
  else if (hit.kind === 'block') row.go = () => openBlock(hit.id, hit.proposition_id);
  // A comment's card is not in the hit, so the board is as close as this gets.
  else row.go = () => { closePalette(); location.hash = ''; };
  return row;
}

// aside is the dim half of a row. The same book saved in two propositions is
// two hits with one title, and the palette drew them as the same row twice, so
// a hit outside the open proposition says which one it is in. A snippet that is
// the title over again says nothing and is left out.
function aside(hit) {
  const parts = [];
  const p = hit.kind === 'proposition' ? null : state.props.find((x) => x.id === hit.proposition_id);
  if (p && p.id !== state.open) parts.push(p.kind === 'show' ? 'Show' : num(p.number) + ' ' + p.title);
  // The ellipsis is where the snippet was cut, so a cut title still matches it.
  const text = (hit.snippet || '').replace(/…/g, '').trim();
  if (text && !hit.title.includes(text)) parts.push(hit.snippet);
  return parts.join(' · ');
}

function pane(tab, open) {
  closePalette();
  location.hash = tab;
  material().then(open).catch(() => {});
}

// openBlock opens the document holding a block and puts it on the screen. The
// payload carries every document of this proposition with its blocks, so which
// one it is in is already here.
function openBlock(id, proposition) {
  const doc = state.documents.find((d) => (d.blocks || []).some((b) => b.id === id));
  const p = state.props.find((item) => item.id === proposition);
  closePalette();
  // A block this tab has no document for is one the page was rendered before,
  // so the proposition is loaded again rather than the palette doing nothing.
  if (!doc) {
    location.href = pathFor(p) + (p?.kind === 'show' ? '#notes' : '');
    return;
  }
  state.document = doc.id;
  state.docSource = false;
  state.tab = p?.kind === 'show' ? 'notes' : 'board';
  // The document is under the board, so the pane in the address bar goes with
  // the pane on the screen.
  location.hash = p?.kind === 'show' ? 'notes' : '';
  emit();
  requestAnimationFrame(() => $(`#doc .blk[data-b="${id}"]`)?.scrollIntoView({ block: 'center' }));
}

// list draws the rows: what this tab knows, filtered the way it always was,
// then what the server found, in the order its groups come back in.
//
// keep is for the one caller that redraws a list somebody is already looking
// at, the reply arriving under them. Every other way here is a new list, and a
// new list starts at the top: opening the palette, and each key press, which is
// a different set of rows even when it has the same number of them.
function list(query, groups, keep = false) {
  const old = $$('#palette ul a');
  const focus = redrawFocus(old, document.activeElement, keep);
  const q = query.toLowerCase();
  // A proposition this tab already lists is not offered twice.
  const known = new Set(state.props.map((p) => p.id));
  const rows = [
    ...local().filter((r) => !q || r.label.toLowerCase().includes(q)),
    ...groups.flatMap((g) => g.hits || [])
      .filter((h) => !(h.kind === 'proposition' && known.has(h.id)))
      .map(remote),
  ];

  const results = clear($('#palette ul'));
  if (!rows.length) {
    results.append(el('li', { class: 'none', text: 'No matches.' }));
    if (focus.restore) $('#palette input').focus();
    return;
  }
  for (const row of rows) {
    const line = row.go
      ? el('a', { href: '#', onclick: (e) => { e.preventDefault(); row.go(); } })
      : el('span', {});
    // The pointer moves the selection, so the row under the cursor and the row
    // Enter opens are one row and not two. It does not scroll: the list moving
    // under a hand that is not moving is the list fighting whoever is reading.
    if (row.go) line.addEventListener('mouseover', () => select(line, false));
    line.append(
      el('span', { class: 'k mono', text: row.kind }),
      el('span', {}, row.label,
        row.snippet ? el('span', { class: 'dim', text: ' · ' + row.snippet }) : null));
    results.append(el('li', { class: row.go ? null : 'flat' }, line));
  }
  const lines = $$('#palette ul a');
  const selected = lines[Math.min(Math.max(focus.index, 0), lines.length - 1)];
  select(selected);
  if (focus.restore) (selected || $('#palette input')).focus();
}

// select marks the row Enter opens, and brings it into view unless the pointer
// is what moved it there.
function select(line, scroll = true) {
  for (const a of $$('#palette ul a')) a.classList.toggle('on', a === line);
  if (line && scroll) line.scrollIntoView({ block: 'nearest' });
}

// search asks the server once the typing has stopped, and draws the local rows
// straight away so the list never goes blank while it waits. An empty query
// asks nobody: there is nothing to match on.
function search(query) {
  clearTimeout(timer);
  const mine = ++asked;
  list(query, []);
  if (!query.trim()) return;
  timer = setTimeout(async () => {
    let groups = [];
    try {
      const answer = await api.get(`/search?q=${encodeURIComponent(query)}&limit=${perKind}`);
      groups = answer.groups || [];
    } catch {
      // Offline, or a session that has ended. The local rows are still there,
      // a session that ended is already on its way to the sign in page, and a
      // line on every key press would be the app shouting.
    }
    if (mine !== asked || $('#palette').hidden) return;
    list(query, groups, true);
  }, pause);
}

$('#palette input').addEventListener('input', (e) => search(e.target.value));
$('#palette').addEventListener('click', (e) => {
  if (e.target.id === 'palette') closePalette();
});
$('#palette').addEventListener('keydown', (e) => {
  if (e.key !== 'Tab') return;
  const stops = [$('#palette input'), ...$$('#palette ul a')];
  const first = stops[0];
  const last = stops[stops.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
});
$('#palette input').addEventListener('keydown', (e) => {
  const lines = $$('#palette ul a');
  if (!lines.length) return;
  const at = lines.findIndex((a) => a.classList.contains('on'));
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    select(lines[Math.min(at + 1, lines.length - 1)]);
  } else if (e.key === 'ArrowUp') {
    e.preventDefault();
    select(lines[Math.max(at - 1, 0)]);
  } else if (e.key === 'Enter') {
    e.preventDefault();
    (lines[at] || lines[0]).click();
  }
});
