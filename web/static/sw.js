// The service worker. It is served from the site root by the server, which
// prepends two lines: VERSION, the hash the static tree is served under, and
// ASSETS, every file in that tree. The cache is named after the version, so a
// deploy changes these bytes, the browser installs the new worker, and the old
// cache is deleted whole rather than being reasoned about file by file.
//
// It caches static assets and two pages that carry nothing about anybody: the
// offline page and the session free shell. It never caches an API answer, a
// websocket, or a page rendered with a session on it.

const CACHE = 'theses-' + VERSION;
const PREFIX = '/static/' + VERSION + '/';

// The two pages worth keeping. The shell is the app with an empty payload: the
// board it draws comes out of IndexedDB, so it says nothing about who is
// signed in and can be handed to whoever asks.
const PAGES = ['/offline', '/shell'];

// APP is the URL an offline navigation is answered with the shell rather than
// the offline page: the workspace and one proposition. Anything else, the
// settings pages and the profile among them, is a page this worker has no
// stand-in for and says so.
const APP = /^\/(p\/\d+)?$/;

// THESES_DEV serves every asset under one unchanging path with no-store on it,
// so a worker holding a copy would serve yesterday's module through today's
// edit. In development this one stands aside entirely.
const dev = VERSION === 'dev';

self.addEventListener('install', (e) => {
  if (dev) return;
  e.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    await cache.addAll(PAGES.concat(ASSETS.map((name) => PREFIX + name)));
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (e) => {
  e.waitUntil((async () => {
    for (const name of await caches.keys()) {
      if (name !== CACHE) await caches.delete(name);
    }
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // Signing out takes this device's copy of the workspace with it, because the
  // next person at this machine would otherwise read the board with no session
  // at all. The cache is left alone: every byte in it is the app itself, the
  // same for everybody, and none of it names anyone.
  if (req.method === 'POST' && url.pathname === '/logout') {
    e.waitUntil(forget());
    return;
  }
  if (dev || req.method !== 'GET') return;

  if (url.pathname.startsWith(PREFIX)) {
    e.respondWith(asset(req));
    return;
  }
  if (req.mode === 'navigate') {
    e.respondWith(navigate(req, url));
  }
  // Everything else, /api, /app, /ws and every write, goes to the network
  // untouched. None of it may be cached: the answers carry the session.
});

// asset is cache first, because a hashed URL names one version of one file for
// ever. A miss is fetched and kept, which is what picks up a file the install
// did not reach.
async function asset(req) {
  const hit = await caches.match(req);
  if (hit) return hit;
  const res = await fetch(req);
  if (res.ok && res.type === 'basic') {
    const cache = await caches.open(CACHE);
    await cache.put(req, res.clone());
  }
  return res;
}

// navigate is network first and falls back only when the network refused to
// answer at all. A 404 or a 500 is the server talking and is passed through:
// answering those from the cache would hide a real page behind a stale one.
async function navigate(req, url) {
  try {
    return await fetch(req);
  } catch {
    const hit = await caches.match(APP.test(url.pathname) ? '/shell' : '/offline');
    return hit || new Response('Offline.', {
      status: 503,
      headers: { 'Content-Type': 'text/plain; charset=utf-8' },
    });
  }
}

// forget drops the cached proposition, the outbox and the uploads waiting to
// start. Everything anybody wrote is in there.
function forget() {
  try {
    indexedDB.deleteDatabase('theses-offline');
  } catch {
    // A page still holding the database blocks the delete. Every store in it is
    // written over on the next sign in either way.
  }
  return Promise.resolve();
}
