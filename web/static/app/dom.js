// Nodes, not strings. Every title, note and name on this board was typed by
// somebody, and building the page out of elements means none of it is ever
// parsed as markup.

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

export function el(tag, attrs, ...kids) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else if (v === true) node.setAttribute(k, '');
    else node.setAttribute(k, v);
  }
  add(node, kids);
  return node;
}

export function add(node, kids) {
  for (const kid of kids.flat(3)) {
    if (kid === null || kid === undefined || kid === false) continue;
    node.append(kid);
  }
  return node;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

// initials is the colored square a person appears as. The color is a class
// because the CSP has no unsafe-inline in style-src.
export function initials(person, extra = '') {
  return el('span', { class: ('u ' + person.colour + ' ' + extra).trim(), title: person.name, text: person.initials });
}

export function num(n) {
  return n < 10 ? '0' + n : String(n);
}

// Mentions are @handle, the account name people sign in with.
const MENTION = /@([a-z0-9][a-z0-9-]*)/g;

// inline renders the little markdown the cards and notes use: bold, italic,
// links, mentions and the two bracketed notes the show writes to itself, an
// aside addressed to somebody and a check to come back to.
//
// The link alternative comes before the note so that [AL: read this](url) is a
// link with an odd label rather than an aside, which is what the server makes
// of the same text. The scheme is in the pattern, so a target that is not http
// or https never matches and the whole of it stays the text somebody typed. A
// target may hold balanced parentheses, because half of Wikipedia's addresses
// do and goldmark reads them that way. The label is bounded so that a line of
// nothing but open brackets is linear work rather than quadratic. The groups
// are named because the order of the alternatives is a reading decision and
// numbering them makes it one more thing to keep in step.
//
// Where this and internal/markdown still differ, deliberately:
//   - a bare URL in the text is a link on the server, which runs goldmark's
//     Linkify, and plain text here;
//   - tables, footnotes, strikethrough and code fences are the server's alone,
//     because the live view renders a paragraph at a time;
//   - a note carries data-by on the server and only its text here.
const INLINE = /\*\*(?<bold>.+?)\*\*|\*(?<italic>.+?)\*|\[(?<label>[^\]\n]{1,512})\]\((?<href>https?:\/\/(?:[^\s()]|\([^\s()]*\))+)\)|\[(?<by>[A-Z]{2,4}): (?<aside>[^\]]+)\]|\[(?<check>check[^\]]*)\]|@(?<handle>[a-z0-9][a-z0-9-]*)/g;

export function inline(text, lookup) {
  const out = [];
  INLINE.lastIndex = 0;
  let at = 0;
  for (let m; (m = INLINE.exec(text)); ) {
    if (m.index > at) out.push(text.slice(at, m.index));
    at = m.index + m[0].length;
    const g = m.groups;
    if (g.bold) out.push(el('b', { text: g.bold }));
    else if (g.italic) out.push(el('i', { text: g.italic }));
    // A label is inline markdown too, the way the server reads it. It cannot
    // hold a closing bracket, so it cannot hold a second link and this goes one
    // deep and no further.
    else if (g.href) out.push(add(el('a', { href: g.href, rel: 'noopener' }), [inline(g.label, lookup)]));
    else if (g.aside) out.push(el('mark', { class: 'note', text: g.by + ': ' + g.aside }));
    else if (g.check) out.push(el('mark', { class: 'note', text: g.check }));
    else {
      const person = lookup(g.handle);
      out.push(person
        ? el('b', { class: 'mention ' + person.colour, text: '@' + g.handle })
        : m[0]);
    }
    // The call above for a link's label shares this regex, so where the outer
    // scan had got to is put back.
    INLINE.lastIndex = at;
  }
  if (at < text.length) out.push(text.slice(at));
  return out;
}

// handles pulls the account names out of a line, which is how the inline card
// form assigns somebody by typing their name.
export function handles(text) {
  return [...new Set([...text.matchAll(MENTION)].map((m) => m[1]))];
}

export function stripHandles(text) {
  return text.replace(/\s*@[a-z0-9][a-z0-9-]*/g, '').trim();
}

// ask is the only confirmation this app has. A real dialog, never confirm().
export function ask(heading, lead, confirmText) {
  return new Promise((resolve) => {
    const keep = el('button', { class: 'lnk plain', type: 'button', text: 'Keep it' });
    const go = el('button', { class: 'lnk del', type: 'button', text: confirmText });
    const dialog = el('dialog', {},
      el('h3', { text: heading }),
      el('p', { text: lead }),
      el('div', { class: 'acts' }, keep, go));
    let answer = false;
    keep.addEventListener('click', () => dialog.close());
    go.addEventListener('click', () => { answer = true; dialog.close(); });
    dialog.addEventListener('close', () => { dialog.remove(); resolve(answer); });
    document.body.append(dialog);
    dialog.showModal();
  });
}

// say puts a line where somebody can see it, for a refusal that has no field of
// its own to sit under. It borrows the offline bar for four seconds, and saying
// so is what stops the next render taking the line away before it was read.
let until = 0;

export function say(text) {
  const bar = $('#netbar');
  bar.textContent = text;
  bar.hidden = false;
  until = Date.now() + 4000;
  clearTimeout(say.timer);
  say.timer = setTimeout(() => {
    until = 0;
    bar.hidden = navigator.onLine;
    bar.textContent = offlineLine;
  }, 4000);
}

// saying reports whether a line is still on the bar.
export const saying = () => Date.now() < until;

export const offlineLine = 'Offline. Your changes are kept on this device.';

// editable turns a node into a one line editor: Enter commits, Escape puts back
// what was there.
export function editable(node, value, commit) {
  node.contentEditable = 'true';
  node.spellcheck = false;
  node.textContent = value;
  node.focus();
  getSelection().selectAllChildren(node);
  let done = false;
  const finish = (save) => {
    if (done) return;
    done = true;
    node.contentEditable = 'false';
    commit(save ? node.textContent.trim() : null);
  };
  node.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); finish(true); }
    if (e.key === 'Escape') { e.preventDefault(); finish(false); }
  });
  node.addEventListener('blur', () => finish(true), { once: true });
}
