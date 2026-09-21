// node web/upload_test.mjs

import assert from 'node:assert/strict';

// api.js reads the CSRF meta tag when it is imported. No request is made by
// this test; this is the one browser surface the pure identity check needs.
globalThis.document = { querySelector: () => null };

const { sameFile } = await import('./static/app/upload.js');
const saved = { name: 'interview.wav', size: 1200, last_modified: 44 };

assert.equal(sameFile(saved, { name: 'interview.wav', size: 1200, lastModified: 44 }), true);
assert.equal(sameFile(saved, { name: 'other.wav', size: 1200, lastModified: 44 }), false, 'name mismatch');
assert.equal(sameFile(saved, { name: 'interview.wav', size: 1201, lastModified: 44 }), false, 'size mismatch');
assert.equal(sameFile(saved, { name: 'interview.wav', size: 1200, lastModified: 45 }), false, 'date mismatch');
assert.equal(sameFile({}, { name: 'legacy.wav', size: 1200, lastModified: 44 }), true, 'legacy rows remain usable');

console.log('upload reselect identity passes');
