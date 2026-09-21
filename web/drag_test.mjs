import assert from 'node:assert/strict';

globalThis.document = { querySelector: () => null };
globalThis.addEventListener = () => {};
globalThis.getComputedStyle = (node) => node.style;

const { scrollParent } = await import('./static/app/drag.js');
const page = { parentElement: null, style: { overflowY: 'visible' }, scrollHeight: 900, clientHeight: 900 };
const rail = { parentElement: page, style: { overflowY: 'auto' }, scrollHeight: 1200, clientHeight: 500 };
const list = { parentElement: rail, style: { overflowY: 'visible' }, scrollHeight: 1200, clientHeight: 1200 };
const row = { parentElement: list };

assert.equal(scrollParent(row), rail, 'the nearest scrolling panel moves under the pointer');
rail.scrollHeight = rail.clientHeight;
assert.equal(scrollParent(row), null, 'a panel with no overflow leaves scrolling to the window');

console.log('drag scroll container cases pass');
