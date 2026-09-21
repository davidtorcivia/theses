// node web/picker_test.mjs

import assert from 'node:assert/strict';

const range = {
  start: null, collapsed: false,
  setStart(node, at) { this.start = [node, at]; },
  collapse(value) { this.collapsed = value; },
};
const selection = {
  cleared: false, added: null,
  removeAllRanges() { this.cleared = true; },
  addRange(value) { this.added = value; },
};
globalThis.document = {
  querySelector: () => null, removeEventListener: () => {}, createRange: () => range,
};
globalThis.addEventListener = () => {};
globalThis.getSelection = () => selection;
const { canAssign, contenteditableCaret, handleMentionKey, writeContenteditable } = await import('./static/app/picker.js');
const proposition = { members: [2] };
const assigned = new Set([3]);

assert.equal(canAssign({ id: 1, role: 'owner' }, assigned, proposition), true,
  'workspace owners can be assigned');
assert.equal(canAssign({ id: 2, role: 'editor' }, assigned, proposition), true,
  'proposition members can be assigned');
assert.equal(canAssign({ id: 3, role: 'editor' }, assigned, proposition), true,
  'assigned outsiders stay visible so they can be removed');
assert.equal(canAssign({ id: 4, role: 'editor' }, assigned, proposition), false,
  'other outsiders are not offered');

const key = (name) => {
  const event = new Event('keydown', { cancelable: true });
  Object.defineProperty(event, 'key', { value: name });
  return event;
};
const choice = {
  clicked: false,
  getAttribute: (name) => name === 'aria-selected' ? 'true' : '',
  click() { this.clicked = true; },
};
const picker = { querySelectorAll: () => [choice] };
for (const name of ['Enter', 'Escape']) {
  const target = new EventTarget();
  let leaked = false;
  target.addEventListener('keydown', (event) => handleMentionKey(event, picker, {}));
  target.addEventListener('keydown', () => { leaked = true; });
  target.dispatchEvent(key(name));
  assert.equal(leaked, false, name + ' does not reach the field handler registered after mentions');
}
assert.equal(choice.clicked, true, 'Enter chooses the active mention');

const textNode = {};
const editable = {
  firstChild: textNode, focused: false,
  set textContent(value) { this.text = value; },
  focus() { this.focused = true; },
};
writeContenteditable(editable, 'Hello @ada rest', 11);
assert.equal(editable.text, 'Hello @ada rest');
assert.equal(editable.focused, true);
assert.deepEqual(range.start, [textNode, 11], 'contenteditable caret follows the inserted mention');
assert.equal(range.collapsed, true);
assert.equal(selection.cleared, true);
assert.equal(selection.added, range);

const nested = {};
const measure = {
  selected: null, end: null,
  selectNodeContents(node) { this.selected = node; },
  setEnd(node, at) { this.end = [node, at]; },
  toString: () => 'first\nsecond'.slice(0, 9),
};
const multi = { textContent: 'first\nsecond', contains: (node) => node === nested };
const live = { startContainer: nested, startOffset: 2, cloneRange: () => measure };
assert.equal(contenteditableCaret(multi, { rangeCount: 1, getRangeAt: () => live }), 9,
  'a nested contenteditable caret is measured from the start of the whole field');
assert.equal(measure.selected, multi);
assert.deepEqual(measure.end, [nested, 2]);

console.log('picker behavior passes');
