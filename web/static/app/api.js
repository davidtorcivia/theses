// The browser's half of the links and files routes. The board goes over the
// websocket, because every board command is small and its answer is an event;
// these are requests with answers of their own, a presigned URL or a list, and
// they go over fetch to the same commands under /app.

import * as offline from './offline.js';
import { isJSON, sessionEnded } from './response.js';

// The CSRF token this page was rendered with. Every request that is not a read
// carries it in a header, because these bodies are JSON and have no form field
// to put it in.
const token = document.querySelector('meta[name="csrf"]')?.content || '';

export class Refused extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

async function call(method, path, body, extra) {
  const headers = { Accept: 'application/json', ...extra };
  if (method !== 'GET') {
    headers['Content-Type'] = 'application/json';
    headers['X-CSRF-Token'] = token;
  }
  let res;
  try {
    res = await fetch('/app' + path, {
      method,
      signal: AbortSignal.timeout(path === '/drive/import' ? 30 * 60 * 1000 : 30000),
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new Refused(method === 'GET' ? 'Could not connect. Retry when connected.' : 'The response was lost. The change may have saved; retry the same request to confirm.', 0);
  }
  if (res.status === 204) return null;
  // A session that has run out is a redirect to the sign-in page, which fetch
  // follows and hands back as a successful page of HTML. Answering null to the
  // caller would be a crash three lines later, so it is a refusal here.
  if (!isJSON(res)) {
    // Only our own sign-in page means the session has ended, and only that is
    // worth throwing this device's work away for. Anything else that answers a
    // read with a page is something in the way, a captive portal, a proxy's
    // block page, a maintenance notice, and the session behind this browser is
    // very likely still good.
    if (sessionEnded(res, location.href)) {
      offline.signedOut();
      throw new Refused('Your session has ended. Sign in again.', 401);
    }
    throw new Refused(method === 'GET' ? 'Could not read the response.' : 'The response could not be confirmed. Retry the same request.', method === 'GET' ? res.status : 0);
  }
  let payload = null;
  try {
    payload = await res.json();
  } catch {
    throw new Refused(method === 'GET' ? 'Could not read the response.' : 'The response could not be confirmed. Retry the same request.', method === 'GET' ? res.status : 0);
  }
  if (!res.ok) {
    throw new Refused((payload && payload.error) || 'That did not go through.', res.status);
  }
  return payload;
}

export const get = (path) => call('GET', path);

// post takes extra headers, which is how a replay out of the outbox names the
// change it is making: a request whose answer never came back goes again under
// the same Idempotency-Key and adds one link rather than two.
export const post = (path, body, extra) => call('POST', path, body ?? {}, extra);
export const patch = (path, body) => call('PATCH', path, body);
export const del = (path) => call('DELETE', path);

// replace is the one document command that comes this way rather than over the
// socket: writing a whole document back from its markdown answers with the
// blocks its paragraphs now stand on, which is not an event. It takes extra
// headers the way post does, because an attempt that was never answered goes
// again under the same Idempotency-Key.
export const replace = (path, body, extra) => call('PUT', path, body, extra);

// put sends bytes straight to the bucket with the headers the server signed
// into the URL. Host and Content-Length are on that list because they are part
// of the signature, and the browser sets both itself and refuses to be told.
// It resolves with the ETag, which is what a multipart part is assembled by.
export async function put(url, headers, body, onProgress) {
  const send = new Headers();
  for (const [k, v] of Object.entries(headers || {})) {
    if (/^(host|content-length)$/i.test(k)) continue;
    send.set(k, v);
  }
  // XMLHttpRequest rather than fetch, because fetch cannot report how far an
  // upload has got and a six gigabyte recording needs a progress bar.
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', url, true);
    let stalled=false, timer;
    const watch=()=>{clearTimeout(timer);timer=setTimeout(()=>{stalled=true;xhr.abort();},120000);};
    xhr.addEventListener('loadend',()=>clearTimeout(timer));
    for (const [k, v] of send) xhr.setRequestHeader(k, v);
    xhr.upload.addEventListener('progress', (e) => {
      watch();
      if (onProgress && e.lengthComputable) onProgress(e.loaded, e.total);
    });
    xhr.addEventListener('load', () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(xhr.getResponseHeader('ETag') || '');
        return;
      }
      reject(new Refused(bucketTrouble(xhr.status), xhr.status));
    });
    xhr.addEventListener('error', () =>
      reject(new Refused(
        'The bucket refused the upload. Check its CORS rule on the Storage settings page.', 0)));
    xhr.addEventListener('abort', () => reject(new Refused(stalled?'The upload stopped making progress. Retry to resume it.':'That upload was stopped.', 0)));
    watch();
    xhr.send(body);
  });
}

function bucketTrouble(status) {
  if (status === 403) return 'The bucket refused that upload. Its keys or its CORS rule need a look.';
  return 'The bucket answered ' + status + '. The upload did not finish.';
}

// An uncertain response keeps both the key and payload until a retry answers.
export function mutation() {
  let pending;
  return {
    get pending() { return Boolean(pending); },
    async run(method, path, body) {
      pending ||= { method, path, body: structuredClone(body), key: crypto.randomUUID() };
      try {
        const out = await call(pending.method, pending.path, pending.body, { 'Idempotency-Key': pending.key });
        pending = null;
        return out;
      } catch (err) {
        if (err.status >= 400 && err.status < 500 && err.status !== 408) pending = null;
        throw err;
      }
    },
  };
}
