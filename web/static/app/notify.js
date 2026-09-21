// The notification matrix saves itself. Everything on these two pages works
// without this file: the matrix has a Save button, every channel form is a
// form, and every test is a post. This only spares somebody ticking twelve rows
// of boxes and then having to remember the button underneath them.

import { $, el } from './dom.js';
import * as offline from './offline.js';
import { sessionEnded } from './response.js';

export function notificationOutcome(res, base) {
  if (sessionEnded(res, base)) return 'signed-out';
  const answered = new URL(res.url || '', base);
  const origin = new URL(base).origin;
  if (res.ok && answered.origin === origin && answered.pathname === '/profile' &&
      answered.searchParams.get('saved') === 'notifications') return 'saved';
  return 'failed';
}

const form = $('form[data-notify-matrix]');
if (form) {
  const status = el('span', { class: 'mono dim', role: 'status', 'aria-live': 'polite', text: '' });
  const save = $('button.save', form);
  if (save) save.after(status);

  let timer = null;
  let sending = false;
  let again = false;

  form.addEventListener('change', (e) => {
    if (e.target.name !== 'rule') return;
    status.textContent = 'Saving…';
    clearTimeout(timer);
    // One request for a run of ticks: a whole column is one save, not eight.
    timer = setTimeout(send, 400);
  });

  async function send() {
    if (sending) { again = true; return; }
    sending = true;
    try {
      const res = await fetch(form.action, {
        method: 'POST',
        body: new URLSearchParams([...new FormData(form)]),
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      });
      const outcome = notificationOutcome(res, location.href);
      if (outcome === 'signed-out') {
        again = false;
        offline.signedOut();
        return;
      }
      status.textContent = outcome === 'saved' ? 'Saved.' : 'Not saved. Press Save.';
    } catch {
      status.textContent = 'Not saved. You are offline.';
    } finally {
      sending = false;
      if (again) { again = false; send(); }
    }
  }
}
