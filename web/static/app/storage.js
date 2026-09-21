// The Storage settings page's CORS check. The server can write to the bucket
// with its own keys and still leave the browser locked out, because a browser
// asks the bucket for permission first and the server never does. So this runs
// the real thing: ask for a presigned PUT, send it from this page, and say
// which way it went.

import * as offline from './offline.js';
import { isJSON, sessionEnded } from './response.js';

const result = document.getElementById('corsresult');
const token = document.querySelector('meta[name="csrf"]')?.content || '';

function show(text, bad) {
  result.textContent = text;
  result.hidden = false;
  result.classList.toggle('bad', Boolean(bad));
}

async function ask(prefix, done) {
  const body = new URLSearchParams({ csrf: token, prefix });
  if (done) body.set('done', '1');
  const res = await fetch('/settings/test/cors', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' },
    body,
  });
  if (sessionEnded(res, location.href)) {
    offline.signedOut();
    throw new Error('Your session has ended. Sign in again.');
  }
  const payload = isJSON(res) ? await res.json().catch(() => null) : null;
  if (!res.ok) {
    throw new Error((payload && payload.error) || 'That bucket could not be reached.');
  }
  return payload;
}

async function check(button) {
  const prefix = button.dataset.cors;
  const was = button.textContent;
  button.disabled = true;
  button.textContent = 'Checking…';
  try {
    const probe = await ask(prefix, false);
    const headers = new Headers();
    for (const [k, v] of Object.entries(probe.headers || {})) {
      // Host and Content-Length are signed but the browser sets both itself.
      if (/^(host|content-length)$/i.test(k)) continue;
      headers.set(k, v);
    }
    let res;
    try {
      res = await fetch(probe.url, { method: 'PUT', headers, body: probe.body });
    } catch {
      show('The browser could not reach the bucket. That is what a missing CORS rule looks like: ' +
        'apply the rule below, allowing PUT, GET and HEAD from ' + probe.origin + '.', true);
      return;
    }
    if (!res.ok) {
      show('The bucket allowed the request and then refused it with ' + res.status +
        '. The CORS rule is in place; the keys or the bucket name are not right.', true);
      return;
    }
    show('The browser uploaded a probe object to the bucket and it was accepted. ' +
      'Uploads from ' + probe.origin + ' will work.');
  } catch (err) {
    show(err.message, true);
  } finally {
    button.disabled = false;
    button.textContent = was;
    // The probe object goes whichever way the check went, and a failure to
    // tidy up is not worth putting on the page: there is nothing there.
    ask(prefix, true).catch(() => {});
  }
}

for (const button of document.querySelectorAll('[data-cors]')) {
  button.addEventListener('click', () => check(button));
}


import * as api from './api.js';
import { el, clear, ask as confirmAction } from './dom.js';
const inspect = document.getElementById('inspect-orphans');
const orphans = document.getElementById('orphan-results');
let orphanBefore = 0;
if (inspect) inspect.onclick = async () => {
  inspect.disabled = true;
  clear(orphans).append(el('p', { text: 'Inspecting…' }));
  try {
    const { objects, before, more } = await api.get('/storage/orphans?before=' + orphanBefore);
    orphanBefore = more ? before : 0;
    inspect.textContent = more ? 'Inspect older deletions' : 'Inspect unused objects again';
    clear(orphans).append(el('p', { text: `${objects.length} eligible objects. Nothing has been deleted.` }));
    for (const object of objects) {
      const row = el('p', {}, el('span', { text: object.key + ' · ' + object.size + ' bytes · ' }));
      row.append(el('button', { type: 'button', class: 'lnk', text: 'Delete this object', onclick: async (e) => {
        if (!await confirmAction('Delete this unused object?', 'This cannot be undone. References are checked again before deletion.', 'Delete object')) return;
        const button = row.querySelector('button'); button.disabled = true;
        try { await api.post('/storage/cleanup', object); row.textContent = 'Deleted ' + object.key; }
        catch (err) { row.append(el('span', { text: err.message })); button.disabled = false; }
      } }));
      orphans.append(row);
    }
  } catch (err) { clear(orphans).append(el('p', { text: err.message })); }
  finally { inspect.disabled = false; }
};
