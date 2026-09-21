// node web/source_test.mjs

import assert from 'node:assert/strict';
import { retainReplay, replayNotice } from './static/app/source.js';

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
