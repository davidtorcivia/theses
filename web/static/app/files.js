// The files pane and the file drawer: drop a file and it goes straight to the
// bucket, folders as facets, versions, and a download that is a presigned GET.

import { $, el, clear, initials, say, ask, editable } from './dom.js';
import { state, user, emit, hold, canEdit, material } from './state.js';
import { when, copy } from './links.js';
import * as api from './api.js';
import * as upload from './upload.js';
import { driveButton } from './drive.js';

// The folder a dropped file lands in when the pane is showing all of them.
const DEFAULT_FOLDER = 'Documents';

export function renderFiles(pane) {
  material().catch((err) => say(err.message));
  if (!asked) { asked = true; resumeWhatIsLeft(); }

  const rows = matching();
  pane.append(el('div', { class: 'ph' },
    el('h2', { text: 'Files' }),
    el('span', { id: 'fsum', class: 'mono', text: `${rows.length} of ${state.files.length} · ${bytes(total())}` })));

  if (canEdit()) pane.append(dropZone());

  pane.append(el('div', { class: 'tools' },
    el('input', {
      id: 'fq', class: 'q', type: 'search', spellcheck: 'false', value: state.fileQuery,
      placeholder: 'Search file names',
      oninput: (e) => { state.fileQuery = e.target.value; redraw('#fq'); },
    }),
    folderFacets()));

  const list = el('ul', { id: 'flist', class: 'list' });
  // The files waiting for a connection sit at the top with nothing but a name:
  // there is no row on the server to draw yet, and there will not be one until
  // the bytes have gone.
  for (const [id, held] of state.uploads) {
    if (!held.queued) continue;
    list.append(el('li', { class: 'row pending', 'data-id': id },
      el('span', { class: 'k mono', text: 'waiting' }),
      el('div', { class: 'main' }, el('span', { class: 't', text: held.name })),
      el('span', { class: 'when mono', text: 'goes up when the connection is back' })));
  }
  if (!rows.length) {
    // A read that failed is not the same as there being none, and saying the
    // second when the first happened is the app being confidently wrong.
    list.append(el('li', {
      class: 'none',
      text: state.files.length ? 'Nothing matches.'
        : state.materialFailed ? 'These could not be read. Reload to try again.'
          : 'No files yet. Drop one above.',
    }));
  }
  for (const file of rows) list.append(row(file));
  pane.append(list);
}

function dropZone() {
  const picker = el('input', { type: 'file', multiple: true, hidden: true });
  picker.addEventListener('change', () => {
    take([...picker.files]);
    picker.value = '';
  });
  const zone = el('div', { id: 'drop' },
    el('span', { text: 'Drop files anywhere here' }),
    el('span', { class: 'mono' },
      'or ',
      el('button', { class: 'lnk', type: 'button', text: 'choose', onclick: () => picker.click() }),
      ' · files go straight to the bucket, never through this app'),
    el('span', { class: 'mono' },
      driveButton((row) => { put(row); emit(); }),
      ' · a file in Drive is copied through this app into the bucket'),
    picker);
  for (const name of ['dragenter', 'dragover']) {
    zone.addEventListener(name, (e) => { e.preventDefault(); zone.classList.add('over'); });
  }
  zone.addEventListener('dragleave', () => zone.classList.remove('over'));
  zone.addEventListener('drop', (e) => {
    e.preventDefault();
    zone.classList.remove('over');
    take([...e.dataTransfer.files]);
  });
  return zone;
}

// take starts one upload per dropped file, asking what to do about a name that
// is already here.
async function take(chosen) {
  const folder = state.folder === 'all' ? DEFAULT_FOLDER : state.folder;
  for (const file of chosen) {
    const existing = state.files.find((f) => f.name === file.name && f.folder === folder && f.state === 'ready');
    let replace = 0;
    if (existing) {
      const asNew = await ask(
        'There is already a file called ' + file.name + '.',
        'Keep both, or make this one a new version of it? The old version stays and is listed in the drawer.',
        'Replace as a new version');
      if (asNew) replace = existing.id;
    }
    run(file, folder, replace);
  }
}

// run is one upload, with its progress kept in state so the row redraws itself
// as the bytes go.
async function run(file, folder, replace) {
  let id = 0;
  // Nothing can be uploaded with no connection: the bytes go to the bucket, and
  // the bucket is on the far side of the same network. The file waits in the
  // list instead and goes up when there is a line again.
  if (!navigator.onLine) {
    const held = await upload.hold(state.open, state.me, file, folder, replace);
    if (!held) {
      say('This browser will not keep files for later. Add it again when the connection is back.');
      return;
    }
    state.uploads.set(held, { name: file.name, at: 0, queued: true });
    emit();
    return;
  }
  try {
    const ready = await upload.start(state.open, state.me, file, folder, replace, {
      // The row exists before a byte has moved, so the list shows it filling
      // up. The same row arrives on the socket; applying it twice is applying
      // it once, because every payload is the whole row.
      started: (row) => { id = row.id; put(row); progress(id, file.name, 0); },
      progress: (fraction) => progress(id, file.name, fraction),
    });
    state.uploads.delete(ready ? ready.id : id);
    if (ready) put(ready);
    emit();
  } catch (err) {
    say(err.message);
    if (id) state.uploads.set(id, { name: file.name, at: 0, error: err.message });
    emit();
  }
}

function progress(id, name, fraction) {
  if (!id) return;
  state.uploads.set(id, { name, at: fraction });
  emit();
}

function put(row) {
  const at = state.files.findIndex((f) => f.id === row.id);
  if (at < 0) state.files.unshift(row); else state.files[at] = row;
}

// resumeWhatIsLeft picks up uploads this browser started and did not finish,
// and starts the ones that were dropped with no connection to start them on.
// The server is asked which parts the bucket already has, so nothing is sent
// twice. It runs once when the pane is first drawn and again the moment the
// browser says there is a network.
let running = false;
let asked = false;
async function resumeWhatIsLeft() {
  if (running || !state.open || !navigator.onLine) return;
  running = true;
  try {
    await carryOn();
  } finally {
    running = false;
  }
}

addEventListener('online', () => { asked = true; resumeWhatIsLeft(); });

async function carryOn() {
  let rows = [];
  try {
    rows = await upload.pending();
  } catch {
    return;
  }
  for (const row of rows) {
    // A note left by whoever was signed in before is not this person's to send.
    if (row.me && state.me && row.me !== state.me) {
      await upload.forget(row.file);
      continue;
    }
    if (row.proposition !== state.open) continue;
    const name = row.handle ? row.handle.name : '';
    // A file that waited for a connection has no server row yet, so it starts
    // rather than resumes, and its placeholder leaves the list with it.
    if (row.queued) {
      state.uploads.delete(row.file);
      await upload.forget(row.file);
      if (row.handle) await run(row.handle, row.folder, row.replace);
      emit();
      continue;
    }
    progress(row.file, name, 0);
    try {
      const ready = await upload.resume(row, {
        started: put,
        progress: (fraction) => progress(row.file, name, fraction),
      });
      state.uploads.delete(ready ? ready.id : row.file);
      if (ready) put(ready);
    } catch (err) {
      state.uploads.set(row.file, { name, at: 0, error: err.message });
      // A file the server no longer has is one the sweep took, and one it
      // refuses outright is one it will refuse again: the note goes with both
      // rather than being offered on every reload from now on.
      if (err.status === 404 || err.status === 422) {
        await upload.forget(row.file);
        state.uploads.delete(row.file);
      }
    }
    emit();
  }
}

function folderFacets() {
  const facets = el('div', { id: 'ffacets', class: 'facets' });
  for (const folder of ['all', ...state.folders]) {
    const n = state.files.filter((f) => f.folder === folder).length;
    facets.append(el('button', {
      type: 'button', 'data-k': folder, class: state.folder === folder ? 'on' : '',
      onclick: () => { state.folder = folder; emit(); },
    }, folder, folder === 'all' ? null : el('i', { text: ' ' + n })));
  }
  return facets;
}

function matching() {
  const q = state.fileQuery.trim().toLowerCase();
  return state.files.filter((f) =>
    (state.folder === 'all' || f.folder === state.folder) &&
    (!q || f.name.toLowerCase().includes(q)));
}

function total() {
  return state.files.reduce((sum, f) => sum + (f.size || 0), 0);
}

function row(file) {
  const busy = state.uploads.get(file.id);
  const li = el('li', { class: 'row', 'data-id': file.id },
    el('span', { class: 'k mono', text: (file.kind || 'file').toUpperCase() }),
    el('div', { class: 'main' },
      el('span', { class: 't', text: file.name }),
      el('span', { class: 'src', text: second(file, busy) })),
    el('span', { class: 'size mono', text: bytes(file.size) }),
    el('span', { class: 'by' }, file.uploaded_by ? initials(user(file.uploaded_by)) : null),
    el('span', { class: 'when mono', text: when(file.created_at) }),
    file.state === 'ready'
      ? el('a', { class: 'dl', href: '#', title: 'Download', text: '↓',
          onclick: (e) => { e.preventDefault(); e.stopPropagation(); download(file); } })
      : el('span', { class: 'dl mono', text: busy ? Math.round(busy.at * 100) + '%' : '…' }));
  li.addEventListener('click', () => openFile(file.id));
  return li;
}

function second(file, busy) {
  if (busy && busy.error) return busy.error;
  if (file.state !== 'ready') return 'uploading' + (busy ? ' · ' + Math.round(busy.at * 100) + '%' : '');
  const bits = [file.folder];
  if (file.duration_ms) bits.push(clock(file.duration_ms));
  if (file.width && file.height) bits.push(file.width + ' × ' + file.height);
  return bits.join(' · ');
}

export function bytes(n) {
  if (!n) return '0 KB';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let at = 0;
  while (n >= 1024 && at < units.length - 1) { n /= 1024; at++; }
  return (at === 0 || n >= 10 ? Math.round(n) : n.toFixed(1)) + ' ' + units[at];
}

function clock(ms) {
  const all = Math.round(ms / 1000);
  const minutes = Math.floor(all / 60);
  const seconds = String(all % 60).padStart(2, '0');
  return minutes + ':' + seconds;
}

export function openFile(id) {
  state.openFile = id;
  state.openCard = null;
  state.openLink = null;
  emit();
}

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

async function download(file) {
  try {
    const { url } = await api.get('/files/' + file.id + '/download');
    // A presigned GET with the file's name signed into its disposition, so the
    // browser saves it under the name the list shows.
    location.href = url;
  } catch (err) {
    say(err.message);
  }
}

// The drawer.

export function renderFileDrawer(drawer) {
  const file = state.files.find((f) => f.id === state.openFile);
  if (!file) return false;
  const busy = state.uploads.get(file.id);

  drawer.append(el('div', { class: 'dh' },
    el('span', { class: 'mono', text: file.folder + ' · ' + bytes(file.size) + ' · ' +
      (file.state === 'ready' ? 'uploaded ' : 'uploading since ') + when(file.created_at) +
      (file.uploaded_by ? ' by ' + user(file.uploaded_by).name : '') }),
    el('button', { class: 'x', type: 'button', text: 'Close', onclick: close })));

  const heading = el('h2', { text: file.name, spellcheck: 'false' });
  if (canEdit()) {
    heading.addEventListener('click', () => {
      if (heading.isContentEditable) return;
      hold(true);
      editable(heading, file.name, (value) => {
        hold(false);
        if (!value || value === file.name) { emit(); return; }
        save(file, { name: value });
      });
    });
  }
  drawer.append(heading);
  drawer.append(preview(file, busy));
  drawer.append(props(file));

  drawer.append(el('h4', { text: 'Versions' }));
  drawer.append(versions(file));

  drawer.append(el('h4', { text: 'Used in' }));
  drawer.append(usedIn(file));

  const buttons = el('div', { class: 'cf' });
  if (file.state === 'ready') {
    buttons.append(el('button', {
      class: 'lnk', type: 'button', text: 'Download',
      onclick: () => download(file),
    }));
    buttons.append(el('button', {
      class: 'lnk', type: 'button', text: 'Copy link',
      onclick: async (e) => {
        const button = e.currentTarget;
        try {
          const { url } = await api.get('/files/' + file.id + '/download');
          copy(url, button);
        } catch (err) {
          say(err.message);
        }
      },
    }));
  }
  if (state.can.delete) {
    buttons.append(el('button', {
      class: 'lnk del', type: 'button', text: 'Delete',
      onclick: async () => {
        if (!await ask('Delete ' + file.name + '?',
          'The object is removed from the bucket. This one cannot be undone.', 'Delete it')) return;
        try {
          await api.del('/files/' + file.id);
          state.files = state.files.filter((f) => f.id !== file.id);
          state.openFile = null;
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
  state.openFile = null;
  emit();
}

// preview is what the browser can show without the app ever holding the bytes:
// an image from its thumbnail, audio from a presigned URL, and a line for
// everything else.
function preview(file, busy) {
  if (file.state !== 'ready') {
    const at = busy ? Math.round(busy.at * 100) : 0;
    return el('div', { class: 'preview docp' },
      el('p', { class: 'mono', text: busy && busy.error ? busy.error : 'Uploading · ' + at + '%' }));
  }
  const kind = (file.kind || '').toLowerCase();
  if (['png', 'jpg', 'jpeg', 'gif'].includes(kind) && file.width) {
    const box = el('div', { class: 'preview img' });
    api.get('/files/' + file.id + '/thumb')
      .then(({ url }) => box.append(el('img', { src: url, alt: file.name, class: 'imgbox' })))
      .catch(() => box.append(el('p', { class: 'mono', text: 'No preview for this one.' })));
    return box;
  }
  if (['mp3', 'm4a', 'wav', 'ogg', 'flac'].includes(kind)) {
    const box = el('div', { class: 'preview audio' });
    api.get('/files/' + file.id + '/download')
      .then(({ url }) => box.append(el('audio', { controls: true, preload: 'none', src: url })))
      .catch(() => box.append(el('p', { class: 'mono', text: 'No preview for this one.' })));
    return box;
  }
  return el('div', { class: 'preview docp' },
    el('p', { class: 'mono', text: 'No preview for .' + (kind || 'this') + '. Download it to open it.' }));
}

function props(file) {
  const dl = el('dl', { class: 'props' });
  const folder = el('select', { disabled: !canEdit(), onchange: (e) => save(file, { folder: e.target.value }) });
  for (const name of state.folders) {
    folder.append(el('option', { value: name, selected: name === file.folder, text: name }));
  }
  dl.append(el('dt', { text: 'Folder' }), el('dd', {}, folder));
  dl.append(el('dt', { text: 'Kind' }), el('dd', { text: (file.kind || 'file') +
    (file.duration_ms ? ' · ' + clock(file.duration_ms) : '') }));
  dl.append(el('dt', { text: 'Key' }), el('dd', { class: 'mono', text: file.object_key }));
  return dl;
}

// versions lists what this file replaced. The chain is read when the drawer
// opens, because most files have none and the list is the common case.
function versions(file) {
  const list = el('ul', { class: 'linked' });
  if (!file.version_of) {
    list.append(el('li', { class: 'dim', text: 'One version.' }));
    return list;
  }
  list.append(el('li', { class: 'dim', text: 'Reading the older versions…' }));
  api.get('/files/' + file.id + '/versions')
    .then(({ versions: older }) => {
      clear(list);
      for (const v of older) {
        list.append(el('li', {},
          el('button', {
            class: 'lnk', type: 'button',
            text: v.name + ' · ' + bytes(v.size) + ' · ' + when(v.created_at),
            onclick: () => download(v),
          })));
      }
      if (!older.length) list.append(el('li', { class: 'dim', text: 'One version.' }));
    })
    .catch((err) => { clear(list); list.append(el('li', { class: 'dim', text: err.message })); });
  return list;
}

function usedIn(file) {
  const list = el('ul', { class: 'linked' });
  const on = state.attachments.files.filter((j) => j.file_id === file.id);
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
        onclick: () => api.del('/cards/' + join.card_id + '/files/' + file.id).catch((err) => say(err.message)),
      }) : null));
  }
  if (!on.length) list.append(el('li', { class: 'dim', text: 'No cards yet.' }));
  return list;
}

async function save(file, change) {
  try {
    const answer = await api.patch('/files/' + file.id, {
      name: file.name, folder: file.folder, ...change,
    });
    const at = state.files.findIndex((f) => f.id === answer.file.id);
    if (at >= 0) state.files[at] = answer.file;
  } catch (err) {
    say(err.message);
  }
  emit();
}

export function attachedFiles(cardID) {
  return state.attachments.files
    .filter((j) => j.card_id === cardID)
    .map((j) => state.files.find((f) => f.id === j.file_id))
    .filter(Boolean);
}
