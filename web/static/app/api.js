// The browser's half of the links and files routes. The board goes over the
// websocket, because every board command is small and its answer is an event;
// these are requests with answers of their own, a presigned URL or a list, and
// they go over fetch to the same commands under /app.

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

async function call(method, path, body) {
  const headers = { Accept: 'application/json' };
  if (method !== 'GET') {
    headers['Content-Type'] = 'application/json';
    headers['X-CSRF-Token'] = token;
  }
  let res;
  try {
    res = await fetch('/app' + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new Refused('You are offline. That did not go through.', 0);
  }
  if (res.status === 204) return null;
  let payload = null;
  try {
    payload = await res.json();
  } catch {
    // A refusal from outside the handlers, such as the CSRF guard, answers
    // with a page rather than JSON.
  }
  if (!res.ok) {
    throw new Refused((payload && payload.error) || 'That did not go through.', res.status);
  }
  return payload;
}

export const get = (path) => call('GET', path);
export const post = (path, body) => call('POST', path, body ?? {});
export const patch = (path, body) => call('PATCH', path, body);
export const del = (path) => call('DELETE', path);

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
    for (const [k, v] of send) xhr.setRequestHeader(k, v);
    xhr.upload.addEventListener('progress', (e) => {
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
    xhr.addEventListener('abort', () => reject(new Refused('That upload was stopped.', 0)));
    xhr.send(body);
  });
}

function bucketTrouble(status) {
  if (status === 403) return 'The bucket refused that upload. Its keys or its CORS rule need a look.';
  return 'The bucket answered ' + status + '. The upload did not finish.';
}
