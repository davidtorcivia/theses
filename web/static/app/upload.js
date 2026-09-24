// The upload. Bytes go from the browser to the bucket and never through the
// app: the server hands out presigned URLs and is told afterwards. What is in
// flight is kept in IndexedDB keyed by the file id, so a reload can ask for the
// same file again and pick up where the bucket stopped.

import * as api from './api.js';
import * as offline from './offline.js';

// IndexedDB gets only small metadata. Storing the File itself can consume the
// site's whole quota before the first byte reaches the bucket. The live File
// stays in memory for automatic retries in this tab; after a reload the person
// is asked to choose it again.
const remember = offline.remember;
export const forget = offline.forget;
const handles = new Map();
const newKey = () => crypto.randomUUID ? crypto.randomUUID()
  : [...crypto.getRandomValues(new Uint8Array(16))]
    .map((byte) => byte.toString(16).padStart(2, '0')).join('');

const note = (file) => ({
  name: file.name,
  size: file.size,
  type: file.type || '',
  last_modified: file.lastModified || 0,
});

const withoutHandle = (row) => {
  const clean = { ...row };
  delete clean.handle;
  return clean;
};

export async function pending() {
  const rows = await offline.uploads();
  if(rows===null)throw new api.Refused("Upload recovery storage could not be read. Retry recovery.",0);
  return rows.map((row) => {
    // Read uploads written by the previous release once, then replace their
    // large stored File with metadata when they next resume.
    if (row.handle) handles.set(row.file, row.handle);
    return { ...row, handle: row.handle || handles.get(row.file) || null };
  });
}

// start uploads one file and returns the row the server marked ready. hooks
// takes started, called with the row as soon as it exists so the list can show
// it filling up, and progress, called with a fraction between 0 and 1.
export async function start(proposition, me, file, folder, replace, hooks, queued = null) {
  let idem = queued && queued.idem;
  if (queued && !idem) {
    idem = newKey();
    const kept = await remember({ ...withoutHandle(queued), idem });
    if (kept === null) {
      throw new api.Refused('This browser could not save upload progress. The file was not sent; free browser storage and try again.', 0);
    }
  }
  idem ||= newKey();
  const up = await api.post('/files', {
    proposition,
    name: file.name,
    folder,
    size: file.size,
    replace: replace || 0,
  }, { 'Idempotency-Key': idem });
  const row = { file: up.file.id, folder, proposition, me, replace: replace || 0, ...note(file) };
  handles.set(row.file, file);
  hooks.started(up.file);
  const kept = await remember(row);
  if (kept === null) {
    throw new api.Refused('This browser could not save upload progress. The file was not sent; add it again after freeing browser storage.', 0);
  }
  // The offline placeholder stays recoverable until the server upload is
  // safely recorded under its real id.
  if (queued) {
    await forget(queued.file);
    handles.delete(queued.file);
    if (hooks.remembered) hooks.remembered();
  }
  return carryOn(up, file, hooks.progress);
}

// hold keeps a file that was dropped with no connection. There is no file id
// yet, because only the server gives those out, so the note is filed under a
// negative one of this device's own making until the upload can start. The
// counter is what keeps two files dropped in the same millisecond apart.
let held = 0;

export async function hold(proposition, me, file, folder, replace) {
  const id = -(Date.now() * 1000 + (++held % 1000));
  handles.set(id, file);
  const kept = await remember({ file: id, folder, proposition, me,
    replace: replace || 0, queued: true, idem: newKey(), ...note(file) });
  // A browser that will not keep it cannot promise to send it later, and a file
  // promised and then dropped is worse than one refused out loud.
  if (kept === null) handles.delete(id);
  return kept === null ? 0 : id;
}

// use verifies a reselected file before attaching its bytes to a saved upload.
// name, size and modification time are stable across the browser file picker;
// old rows have only a stored handle and are accepted through resume below.
export async function use(row, file) {
  if (!sameFile(row, file)) {
    throw new api.Refused('Choose the original file: its name, size, or modification time does not match.', 0);
  }
  const saved = { ...withoutHandle(row), ...note(file) };
  handles.set(row.file, file);
  const kept = await remember(saved);
  if (kept === null) {
    handles.delete(row.file);
    throw new api.Refused('This browser could not save that file for resuming. Free browser storage and try again.', 0);
  }
  return { ...saved, handle: file };
}

export function sameFile(row, file) {
  return (!row.name || row.name === file.name)
    && (!Number.isFinite(row.size) || row.size === file.size)
    && (!row.last_modified || row.last_modified === file.lastModified);
}

// resume picks an upload up again from whatever the bucket already holds. The
// file has to be the same one: its size is checked, because a presigned URL was
// signed for that many bytes.
export async function resume(row, hooks) {
  const {file:current}=await api.get('/files/'+row.file);
  if(current.state==='ready'){await forget(row.file);handles.delete(row.file);return {file:current};}
  const file = row.handle || handles.get(row.file);
  const up = await api.get('/files/' + row.file + '/parts');
  if (!file || file.size !== up.file.size) {
    throw new api.Refused('That file has changed since the upload started. Add it again.', 0);
  }
  // Migrate a legacy row that stored the File itself after it has proved to be
  // the right one. If storage is unavailable the old row remains intact.
  if (row.handle) await remember({ ...withoutHandle(row), ...note(file) });
  handles.set(row.file, file);
  hooks.started(up.file);
  return carryOn(up, file, hooks.progress);
}

// carryOn does the work both paths share: one PUT for a small file, or part
// after part for a large one, asking for the next batch of URLs as it goes.
async function carryOn(up, file, onProgress) {
  // Nothing here catches: a failure leaves the note in IndexedDB, which is
  // exactly the case a resume is for, and the server's sweep clears an upload
  // nobody comes back to after two days.
  const id = up.file.id;
  if (up.url) {
    await api.put(up.url, up.headers, file, (sent, total) => onProgress(sent / total));
  } else {
    await parts(up, file, onProgress);
  }
  const done = await api.post('/files/' + id + '/complete', await measure(file));
  await forget(id);
  handles.delete(id);
  return done;
}

// margin is how long before its URLs expire a batch is abandoned for a fresh
// one. A part can take minutes on a slow line, and a URL that expires mid PUT
// comes back as a refusal from the bucket rather than as something to retry.
const margin = 120000;

// deadline is when a batch stops being usable, counted from the moment its
// answer arrived rather than from the expiry time the server put on it. The two
// clocks are not the same one: a browser minutes fast would read every fresh
// batch as already expired and ask for another without ever sending a byte.
function deadline(up) {
  return up.ttl_seconds ? Date.now() + up.ttl_seconds * 1000 : 0;
}

async function parts(up, file, onProgress) {
  const size = blockSize(up);
  const total = Math.ceil(file.size / size);
  let done = new Set(up.done || []);
  let batch = up.parts || [];
  let expires = deadline(up);
  let sent = done.size * size;

  while (done.size < total) {
    if (!batch.length) {
      const next = await api.get('/files/' + up.file.id + '/parts?after=' + highest(done));
      done = new Set(next.done || []);
      batch = next.parts || [];
      expires = deadline(next);
      if (!batch.length) break;
    }
    for (const part of batch) {
      // The batch is dropped rather than finished when its hour is nearly up:
      // the next pass asks for the parts still missing and is signed again,
      // which is also what tells the server the upload is still going.
      if (expires && Date.now() > expires - margin) break;
      const from = (part.number - 1) * size;
      const chunk = file.slice(from, Math.min(from + size, file.size));
      await api.put(part.url, null, chunk, (loaded) => onProgress((sent + loaded) / file.size));
      sent += chunk.size;
      done.add(part.number);
      onProgress(sent / file.size);
    }
    batch = [];
  }
}

// blockSize is the part size the server signed for, which the client must use
// exactly: a part of any other length assembles into a different object.
function blockSize(up) {
  return up.part_size;
}

// highest is the last part number the bucket holds without a gap before it, so
// the next batch starts where this one stopped rather than re-offering parts
// that are already there.
function highest(done) {
  let n = 0;
  while (done.has(n + 1)) n++;
  return n;
}

// measure reads what only the browser knows: how long an audio file runs and
// how large an image is. Both are best effort; the server renders the image
// dimensions itself on completion and takes its own answer over this one.
async function measure(file) {
  const out = { duration_ms: 0, width: 0, height: 0 };
  const url = URL.createObjectURL(file);
  try {
    if (file.type.startsWith('audio/') || file.type.startsWith('video/')) {
      out.duration_ms = Math.round((await duration(url)) * 1000) || 0;
    } else if (file.type.startsWith('image/')) {
      const size = await pixels(url);
      out.width = size.width;
      out.height = size.height;
    }
  } catch {
    // A format this browser will not decode leaves the fields at zero.
  } finally {
    URL.revokeObjectURL(url);
  }
  return out;
}

function duration(url) {
  return new Promise((resolve, reject) => {
    const audio = new Audio();
    audio.preload = 'metadata';
    audio.addEventListener('loadedmetadata', () => resolve(audio.duration));
    audio.addEventListener('error', reject);
    audio.src = url;
  });
}

function pixels(url) {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.addEventListener('load', () => resolve({ width: img.naturalWidth, height: img.naturalHeight }));
    img.addEventListener('error', reject);
    img.src = url;
  });
}
