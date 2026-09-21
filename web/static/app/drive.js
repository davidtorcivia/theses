// Add from Drive. A dialog listing one Drive folder at a time, or the matches
// of a search, and one button that copies the chosen file into this
// proposition. The bytes go from Drive to the bucket through the server, which
// is the one place in this app that happens, so the row appears when the copy
// is finished rather than filling up as it goes.

import { el, clear, ask } from './dom.js';
import { state } from './state.js';
import * as api from './api.js';

const DEFAULT_FOLDER = 'Documents';

export function driveButton(onImported) {
  return el('button', {
    class: 'lnk', type: 'button', text: 'add from Drive',
    onclick: () => open(onImported),
  });
}

function open(onImported) {
  // The stack is where we are: each entry is a Drive folder id and its name,
  // with the root at the bottom.
  const stack = [{ id: '', name: 'Drive' }];
  let query = '';
  let busy = false;

  const list = el('ul', { class: 'list' });
  const where = el('span', { class: 'mono' });
  const folder = el('select', { 'aria-label': 'Which folder it lands in' });
  const note = el('p', { class: 'mono dim' });
  const search = el('input', {
    class: 'q', type: 'search', spellcheck: 'false', placeholder: 'Search Drive',
    oninput: (e) => { query = e.target.value.trim(); redraw(); },
  });
  const dialog = el('dialog', { class: 'drive', 'aria-label':'Add from Drive' },
    el('h3', { text: 'Add from Drive' }),
    el('div', { class: 'tools' }, search, where),
    list,
    note,
    el('div', { class: 'acts' },
      el('label', { class: 'f' }, el('span', { class: 'l', text: 'Into' }), folder),
      el('button', { class: 'lnk plain', type: 'button', text: 'Close', onclick: () => dialog.close() })));
  dialog.addEventListener('close', () => dialog.remove());
  document.body.append(dialog);
  dialog.showModal();

  let timer = 0;
  function redraw() {
    // One request per pause in the typing, not one per keystroke.
    clearTimeout(timer);
    timer = setTimeout(load, 200);
  }

  async function load() {
    if (busy) return;
    busy = true;
    note.textContent = 'Reading Drive…';
    where.textContent = query ? 'search' : stack.map((s) => s.name).join(' / ');
    try {
      const at = stack[stack.length - 1].id;
      const answer = await api.get('/drive?folder=' + encodeURIComponent(at) +
        '&q=' + encodeURIComponent(query));
      fillFolders(answer.folders || []);
      draw(answer.files || []);
      note.textContent = '';
    } catch (err) {
      clear(list);
      note.textContent = err.message;
    } finally {
      busy = false;
    }
  }

  function fillFolders(names) {
    if (folder.options.length) return;
    const here = state.folder && state.folder !== 'all' ? state.folder : DEFAULT_FOLDER;
    for (const name of names) {
      folder.append(el('option', { value: name, text: name, selected: name === here }));
    }
  }

  function draw(rows) {
    clear(list);
    if (stack.length > 1 && !query) {
      list.append(el('li', {},
        el('button', {
          class: 'lnk', type: 'button', text: '← ' + stack[stack.length - 2].name,
          onclick: () => { stack.pop(); load(); },
        })));
    }
    if (!rows.length) {
      list.append(el('li', { class: 'none', text: 'Nothing here that can be imported.' }));
      return;
    }
    for (const row of rows) list.append(entry(row));
  }

  function entry(row) {
    if (row.folder) {
      return el('li', {},
        el('button', {
          class: 'lnk', type: 'button', text: row.name + '/',
          onclick: () => { stack.push({ id: row.id, name: row.name }); query = ''; search.value = ''; load(); },
        }));
    }
    const go = el('button', { class: 'lnk', type: 'button', text: 'Import' });
    const attempt=api.mutation();
    go.addEventListener('click', () => take(row, go, attempt));
    return el('li', {},
      el('span', { text: row.name }),
      el('span', { class: 'mono dim', text: bytes(row.size) }),
      go);
  }

  async function take(row, button, attempt) {
    const since=state.seq;
    const destination=state.open;
    const into = folder.value || DEFAULT_FOLDER;
    const clash = state.files.find((f) => f.name === row.name && f.folder === into && f.state === 'ready');
    if (!attempt.pending && clash && !await ask(
      'There is already a file called ' + row.name + '.',
      'Importing it again adds a second file with the same name. Versions are for files you upload.',
      'Import it anyway')) {
      return;
    }
    button.disabled = true;
    button.textContent = 'Copying…';
    note.textContent = 'Copying ' + row.name + ' into the bucket. Large files take a while.';
    try {
      const answer = await attempt.run('POST','/drive/import', {
        proposition: destination, file: row.id, folder: into,
      });
      note.textContent = row.name + ' is in ' + into + '.';
      button.textContent = 'Imported';
      onImported(answer.file,since);
    } catch (err) {
      note.textContent = err.message;
      button.disabled = false;
      button.textContent = attempt.pending?'Retry import':'Import';
    }
  }

  load();
}

function bytes(n) {
  if (!n) return '';
  const units = ['bytes', 'kB', 'MB', 'GB'];
  let at = 0;
  let size = n;
  while (size >= 1024 && at < units.length - 1) { size /= 1024; at += 1; }
  return (at === 0 ? size : size.toFixed(1)) + ' ' + units[at];
}
