// What this device keeps when the server is not there: the commands waiting to
// go, the last look at the proposition that was open, and the uploads that have
// not started. One database, three stores.
//
// Every call answers null rather than throwing when storage is blocked, which
// is what a private window does. The app then behaves as it did before any of
// this existed: a change made with the socket down is refused out loud instead
// of being kept.

const DB = 'theses-offline';

// The version is bumped whenever a store is added, because that is what makes
// the browser run the upgrade on a database that already exists.
const VERSION = 2;

const STORES = {
  outbox: { keyPath: 'n', autoIncrement: true },
  snapshot: { keyPath: 'proposition' },
  // The links and files of a proposition are kept apart from its snapshot,
  // because they are read by a different thing at a different time: the board
  // arrives with the page and these two requests later, and only if somebody
  // opens the pane that wants them. Written together, a visit that never opened
  // that pane would overwrite them with the nothing it had.
  material: { keyPath: 'proposition' },
  uploads: { keyPath: 'file' },
};

function open() {
  return new Promise((resolve) => {
    let done = false;
    let timer = 0;
    // answer settles this open once, whichever of the several ways it can end
    // gets there first, and closes a connection that turns up after everyone
    // has stopped waiting for it: holding one open is what blocks the next
    // version from being installed.
    const answer = (db) => {
      if (done) {
        if (db) db.close();
        return;
      }
      done = true;
      clearTimeout(timer);
      resolve(db);
    };

    let req;
    try {
      req = indexedDB.open(DB, VERSION);
    } catch {
      answer(null);
      return;
    }
    req.onupgradeneeded = () => {
      for (const [name, options] of Object.entries(STORES)) {
        if (!req.result.objectStoreNames.contains(name)) {
          req.result.createObjectStore(name, options);
        }
      }
    };
    req.onsuccess = () => {
      // A connection held open is what blocks the next upgrade. Closing on the
      // notice means the tab doing the upgrading gets on with it rather than
      // waiting for this one to be shut by hand.
      req.result.onversionchange = () => req.result.close();
      answer(req.result);
    };
    req.onerror = () => answer(null);
    req.onblocked = () => answer(null);
    // An upgrade another tab is blocking stays pending, and every open made
    // after it queues behind that one and fires nothing at all, so none of the
    // handlers above would ever run. Three seconds and the caller is told there
    // is no storage, which is what there is for as long as the block lasts: the
    // app then behaves as it does in a browser that refuses storage, out loud,
    // rather than waiting for ever with the page half drawn.
    timer = setTimeout(() => answer(null), 3000);
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
// A fold takes the incoming row's idem as well as its arguments, and the caller
// mints a fresh one for every command, so a folded row always goes up under a
// key the server has never seen. It has to: the row it folded into may have
// been in the air when the socket went, the server may have applied it, and
// going up again under that key would be answered with what it already did and
// throw away everything typed since.
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
        // The newest arguments and the newest idem, the oldest base and base
        // text: one change from where the server still is to where this person
        // has got to, under a name the server has not answered before.
        //
        // Whether it counts as sent is the incoming command's to say. An edit
        // folding into a row that went up is a change nobody has sent, under a
        // name nobody has answered; the same command coming back from a socket
        // that died is the one that has.
        store.put({ ...found, args: row.args, idem: row.idem, at: Date.now(),
          sending: Boolean(row.sending) });
        return;
      }
      const add = store.add({ at: Date.now(), ...row, key });
      add.onsuccess = () => { out.n = add.result; };
    };
    return out;
  });
  return filed ? filed.n : 0;
}

// queued is every row the outbox holds, or null when it could not be read: a
// database another connection has taken to a later version, one a tab that has
// not closed is blocking, a transaction that was aborted. A read that did not
// happen is not an empty outbox, and the difference decides whether a command
// nobody has answered still exists, so it is the caller's to make rather than
// this one's to flatten.
export function queued() {
  return withStore('outbox', 'readonly', (store) => store.getAll());
}

export const drop = (n) => withStore('outbox', 'readwrite', (store) => store.delete(n));

// take is one row as the store holds it now, named if it is not named yet. The
// drain reads each row again immediately before it sends it, because the
// arguments may have been written over since the pass began: somebody carrying
// on typing folds the newer text into the row that is about to go, and sending
// the older one would put a superseded edit up and leave the real one to
// conflict with it.
//
// A row filed before commands carried an idem has none, and gets the one passed
// in here rather than at the moment of sending, so that a second attempt at the
// same row goes up under the same name. The reading and the naming are one
// transaction, so the arguments and the name handed back are the pair the store
// held at one instant: reading first and naming after would let a fold write
// its own name in between, and this would put the old name back over it while
// the caller held the old arguments, sending the superseded text under a name
// the server had already answered.
// The row is also marked as being sent, and stays marked until it is dropped or
// refused. A row does not leave the outbox when it goes up: it leaves when the
// answer comes back, and a socket that dies in between leaves it where it is.
// So from here on nobody may treat it as a command that has not happened, which
// is what the two calls below are asked to do and what the mark refuses them.
export function take(n, idem) {
  return withStore('outbox', 'readwrite', (store) => {
    const out = { row: null };
    const req = store.get(n);
    req.onsuccess = () => {
      if (!req.result) return;
      out.row = { ...req.result, idem: req.result.idem || idem, sending: true };
      store.put(out.row);
    };
    return out;
  });
}

// named is the command with this name that is still only waiting, for the two
// calls below. A command that has been refused is not it: it is waiting on a
// person rather than on a connection, and the panel is where it is answered.
// Neither is one the drain has taken, whether it is in the air now or was in
// the air when a socket died: the server may have applied it, so rewriting it
// or dropping it would be deciding something only the answer can decide.
//
// ponytail: it is the same scan queue makes for a fold, with the same ceiling
// and the same reason it is fine at the few rows a person makes by hand.
function named(store, idem, then) {
  const all = store.getAll();
  all.onsuccess = () => {
    const any = all.result.find((r) => r.idem === idem);
    const found = any && !any.refused && !any.sending ? any : null;
    then(found, Boolean(any));
  };
}

// retext writes what somebody has typed into the command that has not gone yet,
// which is how a block the server has not made keeps what is written in it: the
// insert waiting in the outbox is the only place that text can be, because the
// block it belongs to has no id to address a save to. It answers whether there
// was a command there to write into; there is not when the insert is in the air
// on a live socket, and then the text waits in the editor for the ack.
//
// The name is left alone, unlike a fold: this is the same command, carrying
// what it always carried, rather than a second change from where the first one
// left the block. If the server has already applied it, because it was in the
// air when the socket went, the replay is answered with what it did and the
// text typed since goes up as an ordinary save once the real block is here.
// It answers whether it wrote, and whether the command is in the outbox at all.
// A command that is not there has been answered and has left, and the block the
// caller drew for it stands for one the server has already made.
export function retext(idem, text) {
  return withStore('outbox', 'readwrite', (store) => {
    const out = { done: false, filed: false };
    named(store, idem, (row, any) => {
      out.filed = any;
      if (!row) return;
      store.put({ ...row, args: { ...row.args, text }, at: Date.now() });
      out.done = true;
    });
    return out;
  });
}

// unqueue drops the command with this name, which is how a block joined back
// into the one above it before it was ever made stops being made at all. It
// answers whether it was still there to drop: a command the drain has already
// taken is on its way and cannot be called back.
export function unqueue(idem) {
  return withStore('outbox', 'readwrite', (store) => {
    const out = { done: false };
    named(store, idem, (row) => {
      if (!row) return;
      store.delete(row.n);
      out.done = true;
    });
    return out;
  });
}

// dropIfUnchanged is what an answered command leaves the outbox by. A row that
// was written into while it was in flight, because the person carried on typing
// in the same field, is left where it is and goes up on the next pass.
export function dropIfUnchanged(n, at) {
  return withStore('outbox', 'readwrite', (store) => {
    const req = store.get(n);
    req.onsuccess = () => {
      if (!req.result) return;
      if (req.result.at === at) { store.delete(n); return; }
      // Written into while it was in flight, so what it holds now is a change
      // nobody has sent and the mark comes off: it is waiting again.
      if (req.result.sending) store.put({ ...req.result, sending: false });
    };
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
        // The mark comes off with the refusal: nothing was applied, and the row
        // is now waiting on a person rather than on the connection.
        store.put({ ...req.result, refused: why, detail: detail || null, sending: false });
      }
    };
    return null;
  });
}

// file is a command the server has already refused, put in the outbox as
// refused rather than queued. A conflict or a refusal answered on the block
// itself is a question held in one tab, and a tab is a thing that closes; filed
// here it is the same row a refusal during the drain makes, so it survives a
// reload, the panel offers it, and one answer settles both.
//
// A refusal already filed under the same name is written over rather than
// joined: one block has one unanswered question, and the newest words are the
// ones worth keeping. It answers the key the row is filed under, or zero when
// there was no storage to file it in, because the caller has to know whether
// the question outlived the tab before it promises that it did.
//
// It writes over a refusal and over nothing else. A refusal is an answer, so it
// cannot be about a command that is still waiting to go or one the drain has
// taken and may already have had applied: those are answered by the drain, in
// refuse above. The row goes in unmarked for the same reason, since the drain
// leaves a refused row alone and never takes it.
//
// ponytail: the same scan queue and named make, with the same ceiling and the
// same reason it is fine at the few rows a person makes by hand.
export async function file(row, key, why, detail) {
  const filed = await withStore('outbox', 'readwrite', (store) => {
    const out = { n: 0 };
    const all = store.getAll();
    all.onsuccess = () => {
      // By name, and by the name the command itself goes up under: an insert
      // has no key to fold on, because nothing folds two inserts together, so
      // its idem is the only thing that says two refusals are about one block.
      // Without this a refusal the drain made and one filed here would be two
      // rows for one insert, the panel would ask twice, and a discard that
      // dropped one of them would leave the other to draw the paragraph again.
      //
      // A name is only a match when there is one. Rows with no name are the
      // inserts the ordinary path queued, and taking the absence of a name as
      // something two rows have in common would write one question over the
      // first other unnamed one in the store, which is somebody's words.
      const found = all.result.find((r) => r.refused
        && ((key && r.key === key) || (row.idem && r.idem === row.idem)));
      // The time is the row's own, after the spread, so a row read out of the
      // store and filed again is stamped now rather than keeping the moment it
      // first went in: refuse and dropIfUnchanged both answer to that stamp.
      const put = store.put({ ...row, key, sending: false, at: Date.now(),
        refused: why, detail: detail || null, ...(found ? { n: found.n } : {}) });
      put.onsuccess = () => { out.n = put.result; };
    };
    return out;
  });
  return filed ? filed.n : 0;
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

// The links and files beside it, written only by whoever has actually read
// them. Everything else leaves this row alone rather than replacing it with the
// nothing it happens to be holding.
export const keepMaterial = (row) => withStore('material', 'readwrite', (store) => store.put(row));

export const cachedMaterial = (proposition) =>
  withStore('material', 'readonly', (store) => store.get(proposition));

export const cached = (proposition) =>
  withStore('snapshot', 'readonly', (store) => store.get(proposition));

export async function showID() {
  const rows = (await withStore('snapshot', 'readonly', (store) => store.getAll())) || [];
  rows.sort((a, b) => b.at - a.at);
  for (const row of rows) {
    const show = (row.payload?.propositions || []).find((p) => p.kind === 'show');
    if (show) return show.id;
  }
  return 0;
}

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
