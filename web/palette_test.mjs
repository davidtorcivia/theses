// node web/palette_test.mjs

import assert from 'node:assert/strict';
import { redrawFocus } from './static/app/palettefocus.js';

const line = (on = false) => ({ classList: { contains: () => on } });
const lines = [line(true), line(), line()];
assert.deepEqual(redrawFocus(lines, lines[2], true), { restore: true, index: 2 },
  'a focused result keeps its own index when delayed results arrive');
assert.deepEqual(redrawFocus(lines, {}, true), { restore: false, index: 0 },
  'keyboard selection remains selected while focus stays in search');

console.log('palette redraw focus passes');
