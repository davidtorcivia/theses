// The notification matrix saves itself. Everything on these two pages works
// without this file: the matrix has a Save button, every channel form is a
// form, and every test is a post. This only spares somebody ticking twelve rows
// of boxes and then having to remember the button underneath them.

import { $, el } from './dom.js';

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
      status.textContent = res.ok ? 'Saved.' : 'Not saved. Press Save.';
    } catch {
      status.textContent = 'Not saved. You are offline.';
    } finally {
      sending = false;
      if (again) { again = false; send(); }
    }
  }
}
