// node web/diff_test.mjs

import assert from 'node:assert/strict';
import { diff } from './static/app/diff.js';

assert.deepEqual(diff(['one', 'two'], ['one', 'three']), [
  [' ', 'one'], ['-', 'two'], ['+', 'three'],
], 'small comparisons retain the exact line diff');

const oldLines = ['same start', ...Array.from({ length: 600 }, (_, i) => 'old ' + i), 'same end'];
const newLines = ['same start', ...Array.from({ length: 600 }, (_, i) => 'new ' + i), 'same end'];
const large = diff(oldLines, newLines);
assert.deepEqual(large.filter(([mark]) => mark === ' ').map(([, line]) => line),
  ['same start', 'same end'], 'large comparisons retain common edges');
assert.deepEqual(large.filter(([mark]) => mark === '-').map(([, line]) => line),
  oldLines.slice(1, -1), 'large comparisons preserve every removed line');
assert.deepEqual(large.filter(([mark]) => mark === '+').map(([, line]) => line),
  newLines.slice(1, -1), 'large comparisons preserve every added line');

console.log('bounded history diff passes');
