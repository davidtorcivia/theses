// What this device keeps when the server is not there: the commands waiting to
// go, the last look at the proposition that was open, and the uploads that have
// not started. One database, three stores.
//
// Every call answers null rather than throwing when storage is blocked, which
// is what a private window does. The app then behaves as it did before any of
// this existed: a change made with the socket down is refused out loud instead
// of being kept.

const DB = 'theses-offline';
const STORES = {
  outbox: { keyPath: 'n', autoIncrement: true },
  snapshot: { keyPath: 'proposition' },
  uploads: { keyPath: 'file' },
};

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
      for (const [name, options] of Object.entries(STORES)) {
        if (!req.result.objectStoreNames.contains(name)) {
          req.result.createObjectStore(name, options);
        }
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => resolve(null);
    req.onblocked = () => resolve(null);
  });
}

// withStore runs one transaction and answers what the request in it returned.
async function withStore(name, mode, run) {
  const db = await open();
  if (!db) return null;
  try {
    return await new Promise((resolve, reject) => {
      const tx = db.transaction(name, mode);
      const out = run(tx.objectStore(name));
      // A get that matched nothing has an undefined result, and that is the
      // answer rather than a reason to hand the request object back instead.
      tx.oncomplete = () => resolve(out instanceof IDBRequest ? out.result : out);
      tx.onerror = () => reject(tx.error);
      tx.onabort = () => reject(tx.error);
    });
  } catch {
    return null;
  } finally {
    db.close();
  }
}

// The outbox. Keys are given out in order by the store itself, so the order
// they replay in is the order they were made in, across reloads and all.

export const queue = (row) => withStore('outbox', 'readwrite', (store) => store.add({ at: Date.now(), ...row }));

export async function queued() {
  return (await withStore('outbox', 'readonly', (store) => store.getAll())) || [];
}

export const drop = (n) => withStore('outbox', 'readwrite', (store) => store.delete(n));

// refuse marks an entry the server would not take. It stays in the outbox: the
// panel draws it, and it leaves when the person has chosen what to do about it.
export function refuse(n, why, detail) {
  return withStore('outbox', 'readwrite', (store) => {
    const req = store.get(n);
    req.onsuccess = () => {
      if (req.result) store.put({ ...req.result, refused: why, detail: detail || null });
    };
    return null;
  });
}

// The cached proposition. One row per proposition, in the shape the server
// renders into the page, so booting from it is the same boot.

export const keep = (row) => withStore('snapshot', 'readwrite', (store) => store.put(row));

export const cached = (proposition) =>
  withStore('snapshot', 'readonly', (store) => store.get(proposition));

// newest is the proposition this device saw last. It is the only one an offline
// page lets anybody change: the plan makes the others read only from whatever
// was cached, and this is how a page with no server to ask tells them apart.
export async function newest() {
  const rows = (await withStore('snapshot', 'readonly', (store) => store.getAll())) || [];
  let best = null;
  for (const row of rows) if (!best || row.at > best.at) best = row;
  return best ? best.proposition : 0;
}

// The uploads store, which upload.js keeps its notes in.

export const remember = (row) => withStore('uploads', 'readwrite', (store) => store.put(row));
export const forget = (file) => withStore('uploads', 'readwrite', (store) => store.delete(file));

export async function uploads() {
  return (await withStore('uploads', 'readonly', (store) => store.getAll())) || [];
}
