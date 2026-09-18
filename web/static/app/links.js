// The links pane and the link drawer: paste a URL and the server reads the
// page, facet by kind and by question, correct what the page got wrong, and
// copy the citation.

import { $, el, clear, initials, say, ask, editable } from './dom.js';
import { state, user, emit, hold, canEdit, material } from './state.js';
import { send, queueLink } from './net.js';
import * as api from './api.js';

export function renderLinks(pane) {
  material().catch((err) => say(err.message));

  const rows = matching();
  pane.append(el('div', { class: 'ph' },
    el('h2', { text: 'Links' }),
    el('span', { id: 'lsum', class: 'mono', text: `${rows.length} of ${state.links.length}` })));

  if (canEdit()) pane.append(addLine());

  pane.append(el('div', { class: 'tools' },
    el('input', {
      id: 'lq', class: 'q', type: 'search', spellcheck: 'false', value: state.linkQuery,
      placeholder: 'Search titles, authors, notes',
      oninput: (e) => { state.linkQuery = e.target.value; redraw('#lq'); },
    }),
    kindFacets(),
    questionFacets()));

  const list = el('ul', { id: 'llist', class: 'list' });
  if (!rows.length) {
    // A read that failed is not the same as there being none, and saying the
    // second when the first happened is the app being confidently wrong.
    list.append(el('li', {
      class: 'none',
      text: state.links.length ? 'Nothing matches.'
        : state.materialFailed ? 'These could not be read. Reload to try again.'
          : 'No links yet. Paste one above.',
    }));
  }
  for (const link of rows) list.append(row(link));
  pane.append(list);
}

// addLine is the paste field. One URL per line pasted at once is still one
// link: the field takes what was typed and the server reads the page.
function addLine() {
  const field = el('input', {
    id: 'ladd', class: 'addline', spellcheck: 'false',
    placeholder: 'Paste a URL and press Enter.',
  });
  field.addEventListener('keydown', async (e) => {
    if (e.key !== 'Enter') return;
    const url = field.value.trim();
    if (!url) return;
    // Reading the page is the server's job, so with no connection the URL goes
    // in the outbox and the title, the author and the date are fetched on the
    // way back up.
    if (!navigator.onLine) {
      field.value = '';
      const kept = await queueLink(state.open, url);
      say(kept
        ? 'That link is kept on this device. It is read when the connection is back.'
        : 'This browser will not keep it. Paste it again when the connection is back.');
      return;
    }
    field.disabled = true;
    field.value = 'Reading ' + url + '…';
    try {
      const link = await api.post('/links', { proposition: state.open, url });
      // The event arrives over the socket too; applying it here as well is
      // free, because every payload is the whole row.
      const at = state.links.findIndex((l) => l.id === link.id);
      if (at < 0) state.links.unshift(link); else state.links[at] = link;
      state.openLink = link.id;
      state.openCard = state.openFile = null;
      emit();
    } catch (err) {
      say(err.message);
      field.disabled = false;
      field.value = url;
      field.focus();
    }
  });
  return field;
}

function kindFacets() {
  const facets = el('div', { id: 'lfacets', class: 'facets' });
  const kinds = ['all', ...(state.kinds.length ? state.kinds : [])];
  for (const kind of kinds) {
    const n = state.links.filter((l) => l.kind === kind).length;
    facets.append(el('button', {
      type: 'button', 'data-k': kind, class: state.linkKind === kind ? 'on' : '',
      onclick: () => { state.linkKind = kind; emit(); },
    }, kind, kind === 'all' ? null : el('i', { text: ' ' + n })));
  }
  return facets;
}

function questionFacets() {
  const facets = el('div', { id: 'lqs', class: 'facets qs' });
  for (const q of state.questions) {
    facets.append(el('button', {
      type: 'button', 'data-q': q, text: q,
      class: state.linkQuestion === q ? 'on' : '',
      title: label(q),
      onclick: () => { state.linkQuestion = state.linkQuestion === q ? null : q; emit(); },
    }));
  }
  return facets;
}

function label(q) {
  const i = state.questions.indexOf(q);
  return state.questionLabels[i] || q;
}

function matching() {
  const q = state.linkQuery.trim().toLowerCase();
  return state.links.filter((l) =>
    (state.linkKind === 'all' || l.kind === state.linkKind) &&
    (!state.linkQuestion || l.question === state.linkQuestion) &&
    (!q || [l.title, l.author, l.note_md, l.url].join(' ').toLowerCase().includes(q)));
}

function row(link) {
  const li = el('li', { class: 'row', 'data-id': link.id },
    el('span', { class: 'k mono', text: link.kind || 'link' }),
    el('div', { class: 'main' },
      el('a', {
        class: 't', href: link.url, target: '_blank', rel: 'noopener noreferrer',
        text: link.title || shortURL(link.url),
      }),
      el('span', { class: 'src', text: source(link) }),
      link.note_md ? el('p', { class: 'note', text: link.note_md }) : null),
    el('span', { class: 'qq', text: link.question || '' }),
    el('span', { class: 'by' }, link.added_by ? initials(user(link.added_by)) : null),
    el('span', { class: 'when mono', text: when(link.created_at) }));
  li.addEventListener('click', (e) => {
    if (e.target.tagName === 'A') return;
    openLink(link.id);
  });
  return li;
}

function source(link) {
  const parts = [];
  if (link.author) parts.push(link.author);
  if (link.year) parts.push(link.year);
  const who = parts.join(', ');
  return (who ? who + ' · ' : '') + host(link.url);
}

export function host(url) {
  try {
    return new URL(url).hostname.replace(/^www\./, '');
  } catch {
    return url;
  }
}

// How much of a bare URL a row shows when the page it points at gave no title.
// Long enough to tell two links on one site apart, short enough that a row with
// one in it is still a row.
const urlRoom = 48;

// shortURL is that fallback: the host and the path, without the scheme, the
// query or a tail nobody reads. The whole URL in a title is what made the links
// pane wider than the phone it was on.
export function shortURL(url) {
  let short = url;
  try {
    const parsed = new URL(url);
    short = parsed.hostname.replace(/^www\./, '') + (parsed.pathname === '/' ? '' : parsed.pathname);
  } catch {
    // Not a URL this browser can parse, so there is nothing to take off it.
  }
  return short.length > urlRoom ? short.slice(0, urlRoom - 1) + '…' : short;
}

// when is the date a thing was added, in the short form the mockup shows.
export function when(unix) {
  if (!unix) return '';
  const then = new Date(unix * 1000);
  const days = Math.floor((Date.now() - then.getTime()) / 86400000);
  if (days <= 0) return 'today';
  if (days === 1) return 'yesterday';
  return then.toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

export function openLink(id) {
  state.openLink = id;
  state.openCard = null;
  state.openFile = null;
  emit();
}

// redraw keeps the caret where it was: the pane is rebuilt from scratch on
// every change, so a field being typed into is focused again afterwards.
function redraw(selector) {
  emit();
  requestAnimationFrame(() => {
    const field = $(selector);
    if (field && document.activeElement !== field) {
      const at = field.value.length;
      field.focus();
      field.setSelectionRange(at, at);
    }
  });
}

// The drawer.

export function renderLinkDrawer(drawer) {
  const link = state.links.find((l) => l.id === state.openLink);
  if (!link) return false;

  drawer.append(el('div', { class: 'dh' },
    el('span', { class: 'mono', text: (link.kind || 'link') + ' · added ' + when(link.created_at) +
      (link.added_by ? ' by ' + user(link.added_by).name : '') }),
    el('button', { class: 'x', type: 'button', text: 'Close', onclick: close })));

  const heading = el('h2', { text: link.title || shortURL(link.url), spellcheck: 'false' });
  if (canEdit()) {
    heading.addEventListener('click', () => {
      if (heading.isContentEditable) return;
      hold(true);
      editable(heading, link.title, (value) => {
        hold(false);
        if (value === null || value === link.title) { emit(); return; }
        save(link, { title: value });
      });
    });
  }
  drawer.append(heading);
  drawer.append(el('p', { class: 'src' },
    el('a', { href: link.url, target: '_blank', rel: 'noopener noreferrer', text: host(link.url) + ' ↗' })));

  drawer.append(props(link));

  drawer.append(el('h4', { text: 'Note' }));
  drawer.append(note(link));

  drawer.append(el('h4', { text: 'Cite' }));
  drawer.append(el('p', { class: 'cite mono', text: link.citation || '' }));

  drawer.append(el('h4', { text: 'Used in' }));
  drawer.append(usedIn(link));

  const buttons = el('div', { class: 'cf' },
    el('button', {
      class: 'lnk', type: 'button', text: 'Copy citation',
      onclick: (e) => copy(link.citation || '', e.currentTarget),
    }));
  if (canEdit() && state.documents.length) buttons.append(sendToDoc(link));
  if (canEdit()) {
    buttons.append(el('button', {
      class: 'lnk', type: 'button', text: 'Fetch again',
      onclick: async (e) => {
        e.currentTarget.disabled = true;
        try {
          apply(await api.post('/links/' + link.id + '/refetch'));
        } catch (err) {
          say(err.message);
        }
        emit();
      },
    }));
  }
  if (state.can.delete) {
    buttons.append(el('button', {
      class: 'lnk del', type: 'button', text: 'Remove',
      onclick: async () => {
        if (!await ask('Remove this link?', 'The note and the question go with it.', 'Remove it')) return;
        try {
          await api.del('/links/' + link.id);
          state.links = state.links.filter((l) => l.id !== link.id);
          state.openLink = null;
          emit();
        } catch (err) {
          say(err.message);
        }
      },
    }));
  }
  drawer.append(buttons);
  return true;
}

function close() {
  state.openLink = null;
  emit();
}

// sendToDoc puts the citation at the end of one of the proposition's documents.
// It is the same block.insert the document itself sends, so the paragraph
// arrives in every other tab the way any other one does.
function sendToDoc(link) {
  const pick = el('select', { 'aria-label': 'Send the citation to a document' },
    el('option', { value: '', text: 'Send to doc' }));
  for (const doc of state.documents) {
    pick.append(el('option', { value: String(doc.id), text: doc.name }));
  }
  pick.addEventListener('change', async () => {
    const doc = state.documents.find((d) => String(d.id) === pick.value);
    pick.value = '';
    if (!doc) return;
    const blocks = doc.blocks || [];
    const text = link.citation || link.title || link.url;
    try {
      const went = await send('block.insert', {
        document: doc.id,
        after: blocks.length ? blocks[blocks.length - 1].id : 0,
        text,
      });
      // A command that was kept rather than sent has arrived nowhere yet, and
      // saying it has would be the app telling a story.
      say(went ? 'Sent to ' + doc.name + '.'
        : 'Kept on this device. It goes into ' + doc.name + ' when the connection is back.');
    } catch (err) {
      say(err.message);
    }
  });
  return pick;
}

function props(link) {
  const dl = el('dl', { class: 'props' });
  dl.append(el('dt', { text: 'Author' }), field(link, 'author', link.author, 'nobody named'));
  dl.append(el('dt', { text: 'Year' }), field(link, 'year', link.year, 'no date'));

  const kind = el('select', { disabled: !canEdit(), onchange: (e) => save(link, { kind: e.target.value }) });
  for (const k of state.kinds) {
    kind.append(el('option', { value: k, selected: k === link.kind, text: k }));
  }
  dl.append(el('dt', { text: 'Kind' }), el('dd', {}, kind));

  const question = el('select', {
    disabled: !canEdit(),
    onchange: (e) => save(link, { question: e.target.value }),
  });
  question.append(el('option', { value: '', selected: !link.question, text: 'none' }));
  for (const q of state.questions) {
    question.append(el('option', { value: q, selected: q === link.question, text: q + ' · ' + label(q) }));
  }
  dl.append(el('dt', { text: 'Question' }), el('dd', {}, question));
  return dl;
}

// field is one editable line in the drawer's list of properties.
function field(link, name, value, placeholder) {
  const dd = el('dd', {}, value ? document.createTextNode(value) : el('span', { class: 'dim', text: placeholder }));
  if (!canEdit()) return dd;
  dd.addEventListener('click', () => {
    if (dd.isContentEditable) return;
    hold(true);
    clear(dd);
    editable(dd, value || '', (typed) => {
      hold(false);
      if (typed === null || typed === value) { emit(); return; }
      save(link, { [name]: typed });
    });
  });
  return dd;
}

function note(link) {
  const p = el('p', {
    class: 'desc', spellcheck: 'false',
    'data-ph': 'Why does this matter for the episode?',
    text: link.note_md,
  });
  if (!canEdit()) return p;
  p.addEventListener('click', () => {
    if (p.isContentEditable) return;
    hold(true);
    editable(p, link.note_md, (value) => {
      hold(false);
      if (value === null || value === link.note_md) { emit(); return; }
      save(link, { note_md: value });
    });
  });
  return p;
}

// usedIn lists the cards this link is attached to, and offers the ones it is
// not. The picker is the card drawer's, from the other direction.
function usedIn(link) {
  const list = el('ul', { class: 'linked' });
  const on = state.attachments.links.filter((j) => j.link_id === link.id);
  for (const join of on) {
    const card = state.cards.get(join.card_id);
    if (!card) continue;
    list.append(el('li', {},
      el('a', {
        href: '#board', text: card.title,
        onclick: (e) => { e.preventDefault(); location.hash = 'board'; },
      }),
      canEdit() ? el('span', { class: 'dim', text: ' · ' }) : null,
      canEdit() ? el('button', {
        class: 'lnk del', type: 'button', text: 'Detach',
        onclick: () => detach(join.card_id, link.id),
      }) : null));
  }
  if (!on.length) list.append(el('li', { class: 'dim', text: 'No cards yet.' }));
  return list;
}

async function detach(card, link) {
  try {
    await api.del('/cards/' + card + '/links/' + link);
  } catch (err) {
    say(err.message);
  }
}

// save writes one or more fields. Every field the row has goes with it, because
// the command takes the whole set: sending one would clear the rest.
async function save(link, change) {
  const body = {
    title: link.title, author: link.author, year: link.year,
    kind: link.kind, note_md: link.note_md, question: link.question || '',
    ...change,
  };
  try {
    apply(await api.patch('/links/' + link.id, body));
  } catch (err) {
    say(err.message);
  }
  emit();
}

function apply(link) {
  const at = state.links.findIndex((l) => l.id === link.id);
  if (at < 0) state.links.unshift(link); else state.links[at] = link;
}

// copy puts a line on the clipboard and says so on the button, because a copy
// with no sign it happened is a copy somebody does twice.
export async function copy(text, button) {
  const was = button.textContent;
  try {
    await navigator.clipboard.writeText(text);
    button.textContent = 'Copied';
  } catch {
    button.textContent = 'Could not copy';
  }
  setTimeout(() => { button.textContent = was; }, 1500);
}

// attachable is the list a card drawer's picker offers, used from drawer.js.
export function attachedLinks(cardID) {
  return state.attachments.links
    .filter((j) => j.card_id === cardID)
    .map((j) => state.links.find((l) => l.id === j.link_id))
    .filter(Boolean);
}
