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

// initials is the coloured square a person appears as. The colour is a class
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
// mentions and the bracketed note the show writes to itself.
export function inline(text, lookup) {
  const out = [];
  const pattern = /\*\*(.+?)\*\*|\*(.+?)\*|\[([A-Za-z0-9-]+): ([^\]]+)\]|@([a-z0-9][a-z0-9-]*)/g;
  let at = 0;
  for (let m; (m = pattern.exec(text)); ) {
    if (m.index > at) out.push(text.slice(at, m.index));
    if (m[1]) out.push(el('b', { text: m[1] }));
    else if (m[2]) out.push(el('i', { text: m[2] }));
    else if (m[3]) out.push(el('mark', { class: 'note', text: m[3] + ': ' + m[4] }));
    else {
      const person = lookup(m[5]);
      out.push(person
        ? el('b', { class: 'mention ' + person.colour, text: '@' + m[5] })
        : m[0]);
    }
    at = m.index + m[0].length;
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
// its own to sit under.
export function say(text) {
  const bar = $('#netbar');
  bar.textContent = text;
  bar.hidden = false;
  clearTimeout(say.timer);
  say.timer = setTimeout(() => { bar.hidden = navigator.onLine; bar.textContent = offlineLine; }, 4000);
}

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
