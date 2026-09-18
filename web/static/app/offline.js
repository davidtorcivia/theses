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

// queue puts a command in the outbox and answers the key it was filed under, or
// zero when there is no storage to file it in. A command that names a row
// already waiting replaces that row's arguments instead of joining the queue
// behind it: two edits to one title while offline are one edit from the text
// the server has to the text the person stopped at, which is also the only
// version pair the server can merge.
//
// ponytail: finding the row waiting is a scan of the outbox, which is fine at
// the few rows a person makes by hand and would be an index on key if a command
// with a key were ever queued in the hundreds. Commands with no key, which is
// every create, skip the scan.
export async function queue(row, key) {
  const filed = await withStore('outbox', 'readwrite', (store) => {
    const out = { n: 0 };
    if (!key) {
      const add = store.add({ at: Date.now(), ...row });
      add.onsuccess = () => { out.n = add.result; };
      return out;
    }
    const all = store.getAll();
    all.onsuccess = () => {
      const found = all.result.find((r) => r.key === key && !r.refused);
      if (found) {
        out.n = found.n;
        // The newest arguments, the oldest base and base text: one change from
        // where the server still is to where this person has got to.
        store.put({ ...found, args: row.args, at: Date.now() });
        return;
      }
      const add = store.add({ at: Date.now(), ...row, key });
      add.onsuccess = () => { out.n = add.result; };
    };
    return out;
  });
  return filed ? filed.n : 0;
}

export async function queued() {
  return (await withStore('outbox', 'readonly', (store) => store.getAll())) || [];
}

export const drop = (n) => withStore('outbox', 'readwrite', (store) => store.delete(n));

// get is one row as the store holds it now. The drain reads each row again
// immediately before it sends it, because the arguments may have been written
// over since the pass began: somebody carrying on typing folds the newer text
// into the row that is about to go, and sending the older one would put a
// superseded edit up and leave the real one to conflict with it.
export const get = (n) => withStore('outbox', 'readonly', (store) => store.get(n));

// dropIfUnchanged is what an answered command leaves the outbox by. A row that
// was written into while it was in flight, because the person carried on typing
// in the same field, is left where it is and goes up on the next pass.
export function dropIfUnchanged(n, at) {
  return withStore('outbox', 'readwrite', (store) => {
    const req = store.get(n);
    req.onsuccess = () => { if (req.result && req.result.at === at) store.delete(n); };
    return null;
  });
}

// refuse marks an entry the server would not take. It stays in the outbox: the
// panel draws it, and it leaves when the person has chosen what to do about it.
// A row written into since it went up is not marked, because the refusal was of
// something the person has already moved past.
export function refuse(n, at, why, detail) {
  return withStore('outbox', 'readwrite', (store) => {
    const req = store.get(n);
    req.onsuccess = () => {
      if (req.result && req.result.at === at) {
        store.put({ ...req.result, refused: why, detail: detail || null });
      }
    };
    return null;
  });
}

// retry clears a refusal so the row goes up again, which is what the panel's
// try it again offers on a refusal that was nobody's fault.
export function retry(n) {
  return withStore('outbox', 'readwrite', (store) => {
    const req = store.get(n);
    req.onsuccess = () => {
      if (req.result) {
        const row = { ...req.result, at: Date.now() };
        delete row.refused;
        delete row.detail;
        store.put(row);
      }
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

// signedOut is what the server saying there is no session behind this browser
// means for what this device holds: the cached proposition, the outbox and the
// uploads all belong to somebody who is no longer here. It lives with the
// database rather than with the socket because both the socket and the ordinary
// requests find out, and there is one right thing to do about it.
let leaving = false;

export async function signedOut() {
  if (leaving) return;
  leaving = true;
  await wipe();
  location.href = '/login';
}

// wipe drops everything this device holds of the workspace.
export function wipe() {
  return new Promise((resolve) => {
    let req;
    try {
      req = indexedDB.deleteDatabase(DB);
    } catch {
      resolve(false);
      return;
    }
    req.onsuccess = () => resolve(true);
    req.onerror = () => resolve(false);
    // A tab still holding the database blocks the delete. Saying so is better
    // than waiting on it for ever; the caller is on its way to the sign-in page.
    req.onblocked = () => resolve(false);
  });
}

// The uploads store, which upload.js keeps its notes in.

export const remember = (row) => withStore('uploads', 'readwrite', (store) => store.put(row));
export const forget = (file) => withStore('uploads', 'readwrite', (store) => store.delete(file));

export async function uploads() {
  return (await withStore('uploads', 'readonly', (store) => store.getAll())) || [];
}
