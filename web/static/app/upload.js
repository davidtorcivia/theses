// The upload. Bytes go from the browser to the bucket and never through the
// app: the server hands out presigned URLs and is told afterwards. What is in
// flight is kept in IndexedDB keyed by the file id, so a reload picks up where
// it left off by asking the server which parts the bucket is still missing.

import * as api from './api.js';

const DB = 'theses-uploads';
const STORE = 'uploads';

// open is the one place IndexedDB is touched. A browser with storage blocked
// or a private window that refuses gives null, and the upload runs without the
// resume: what is lost is the ability to carry on after a reload, not the
// upload itself.
function open() {
  return new Promise((resolve) => {
    let req;
    try {
      req = indexedDB.open(DB, 1);
    } catch {
      resolve(null);
      return;
    }
    req.onupgradeneeded = () => {
      if (!req.result.objectStoreNames.contains(STORE)) {
        req.result.createObjectStore(STORE, { keyPath: 'file' });
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => resolve(null);
    req.onblocked = () => resolve(null);
  });
}

async function withStore(mode, run) {
  const db = await open();
  if (!db) return null;
  try {
    return await new Promise((resolve, reject) => {
      const tx = db.transaction(STORE, mode);
      const out = run(tx.objectStore(STORE));
      tx.oncomplete = () => resolve(out && out.result !== undefined ? out.result : out);
      tx.onerror = () => reject(tx.error);
      tx.onabort = () => reject(tx.error);
    });
  } catch {
    return null;
  } finally {
    db.close();
  }
}

// remember and forget keep the note of what is in flight. The file handle
// itself is stored: a browser keeps a File across a reload as long as the file
// on disk has not changed, which is what makes a resume possible without asking
// the person to find it again.
export const remember = (row) => withStore('readwrite', (store) => store.put(row));
export const forget = (file) => withStore('readwrite', (store) => store.delete(file));

export async function pending() {
  const rows = await withStore('readonly', (store) => store.getAll());
  return rows || [];
}

// start uploads one file and returns the row the server marked ready. hooks
// takes started, called with the row as soon as it exists so the list can show
// it filling up, and progress, called with a fraction between 0 and 1.
export async function start(proposition, file, folder, replace, hooks) {
  const up = await api.post('/files', {
    proposition,
    name: file.name,
    folder,
    size: file.size,
    replace: replace || 0,
  });
  await remember({ file: up.file.id, handle: file, folder, proposition });
  hooks.started(up.file);
  return carryOn(up, file, hooks.progress);
}

// resume picks an upload up again from whatever the bucket already holds. The
// file has to be the same one: its size is checked, because a presigned URL was
// signed for that many bytes.
export async function resume(row, hooks) {
  const file = row.handle;
  const up = await api.get('/files/' + row.file + '/parts');
  if (!file || file.size !== up.file.size) {
    throw new api.Refused('That file has changed since the upload started. Add it again.', 0);
  }
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
  return done.file;
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
