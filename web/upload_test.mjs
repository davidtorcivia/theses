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

const {resume,pending}=await import('./static/app/upload.js');
const requests=[];
globalThis.fetch=async(path)=>{requests.push(path);return new Response(JSON.stringify({file:{id:9,state:'ready',name:'take.wav'}}),{headers:{'Content-Type':'application/json'}});};
const ready=await resume({file:9},{started:()=>assert.fail('ready file must not restart upload')});
assert.equal(ready.file.state,'ready');
assert.deepEqual(requests,['/app/files/9'],'lost completion is confirmed before requesting parts or local bytes');
await assert.rejects(pending(),/recovery storage could not be read/);
console.log('upload completion confirmation and storage failure pass');
