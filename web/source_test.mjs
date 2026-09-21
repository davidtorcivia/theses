// node web/source_test.mjs

import assert from 'node:assert/strict';
import { retainReplay, replayNotice, sourceAttempt, draftMatches, draftRecord } from './static/app/source.js';

const area = { value: 'My unresolved paragraph', selectionStart: 5 };
const src = {
  text: 'My unresolved paragraph',
  area,
  base: [{ id: 1, version: 2 }],
  notice: '',
};

retainReplay(src, { blocks: [{ id: 1, version: 4 }, { id: 2, version: 1 }] });

assert.equal(src.text, 'My unresolved paragraph', 'the unsent markdown is retained');
assert.equal(src.area, area, 'the live textarea is retained');
assert.equal(src.area.value, 'My unresolved paragraph', 'the textarea value is retained');
assert.deepEqual(src.base, [{ id: 1, version: 4 }, { id: 2, version: 1 }], 'the base advances');
assert.equal(src.notice, replayNotice, 'the replay remains visible above the editor');

console.log('source replay retention passes');

const writing = { draftID: 'first-tab', base: [{id:1,version:1}], text: 'first text', clean: '' };
const attempt = sourceAttempt(writing, 'request-1');
writing.text = 'typed during save';
writing.base[0].version = 2;
assert.equal(sourceAttempt(writing, 'request-2'), attempt);
assert.equal(attempt.text, 'first text');
assert.deepEqual(attempt.base, [{id:1,version:1}]);
const document = {id:5,created_at:123};
const draft = draftRecord(writing, 2, 3, document);
assert.equal(draft.text, 'typed during save');
assert.equal(draft.pending.text, 'first text');
assert.ok(draftMatches(draft, 2, 3, document));
assert.ok(!draftMatches(draft, 4, 3, document));
assert.ok(!draftMatches(draft, 2, 4, document));
assert.ok(!draftMatches(draft, 2, 3, {id:5,created_at:124}));
writing.text = 'later';
assert.equal(draft.text, 'typed during save');
