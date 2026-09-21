// node web/notify_test.mjs

import assert from 'node:assert/strict';

globalThis.document = { querySelector: () => null };
const { notificationOutcome } = await import('./static/app/notify.js');
const base = 'https://theses.test/profile';

assert.equal(notificationOutcome({ ok: true, url: base + '?saved=notifications' }, base), 'saved');
assert.equal(notificationOutcome({ ok: true, url: base + '#notifications' }, base), 'failed');
assert.equal(notificationOutcome({ ok: true, url: 'https://theses.test/login' }, base), 'signed-out');
assert.equal(notificationOutcome({ ok: true, url: 'https://other.test/profile?saved=notifications' }, base), 'failed');

console.log('notification autosave outcomes pass');
