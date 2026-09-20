// node web/undo_test.mjs
//
// undo.js decides what one Ctrl+Z takes back, which is arithmetic on a list and
// a clock and nothing else. Getting it wrong either loses a paragraph somebody
// wanted or hands back a whole afternoon, and neither shows up in a page that
// still renders. This sits beside the two embedded trees, like the blocktext
// test, so it is not served to browsers or kept by the service worker.

import assert from 'node:assert/strict';
import { history, of, reset, move, keep, together, cap } from './static/app/undo.js';

// A snapshot with the caret at the end, which is where typing leaves it.
const end = (text) => ({ text, start: text.length, end: text.length });

// typing is a run of keystrokes, each one a millisecond after the last, with
// the caret where the one before it left it. It is what the editor's input
// handler does, written once here.
function typing(h, texts, kind = 'type', from = 0) {
  let when = from;
  let was = null;
  for (const text of texts) {
    when += 1;
    h.record(end(text), kind, when, was);
    was = { start: text.length, end: text.length };
  }
  return when;
}

{
  const h = history('');
  typing(h, ['T', 'Th', 'The']);
  assert.equal(h.steps(), 2, 'a run of typing is one step over the text it started from');
  assert.deepEqual(h.undo(), { text: '', start: 0, end: 0 }, 'undo takes the whole run back');
  assert.equal(h.canUndo(), false, 'there is nothing before the text it started from');
  assert.deepEqual(h.redo(), end('The'), 'redo puts the run back');
  assert.equal(h.canRedo(), false, 'and there is nothing ahead of it');
}

{
  // A pause longer than together starts a new step, which is what makes "type a
  // sentence, pause, type another, Ctrl+Z twice" give the two sentences back
  // one at a time.
  const h = history('');
  const was = typing(h, ['O', 'On', 'One']);
  typing(h, ['One t', 'One tw', 'One two'], 'type', was + together + 1);
  assert.equal(h.steps(), 3, 'a pause ends the run');
  assert.deepEqual(h.undo(), end('One'), 'the first undo takes back the second sentence');
  assert.deepEqual(h.undo(), { text: '', start: 0, end: 0 }, 'the second takes back the first');
}

{
  // Inserting and deleting are different runs even with no pause between them.
  const h = history('ab');
  const when = typing(h, ['abc', 'abcd']);
  typing(h, ['abc', 'ab'], 'cut', when);
  assert.equal(h.steps(), 3, 'deleting does not join a run of typing');
  assert.deepEqual(h.undo(), end('abcd'), 'undo takes back the deleting alone');
}

{
  // The caret jumping means the person went somewhere else in the block, so
  // what they write there is a change of its own.
  const h = history('one two');
  h.record(end('one two!'), 'type', 1, { start: 7, end: 7 });
  h.record({ text: '!one two!', start: 1, end: 1 }, 'type', 2, { start: 0, end: 0 });
  assert.equal(h.steps(), 3, 'a caret that jumped ends the run');
  assert.deepEqual(h.undo(), end('one two!'), 'and the undo takes back only what was written there');
}

{
  // A script write is the state before it and the state after it, so one undo
  // takes back exactly that write, and the typing that follows does not join
  // it however fast it comes.
  const h = history('- one');
  h.record(end('- one'), 'step', 10);
  h.record(end('- one\n- '), 'step', 10);
  h.record(end('- one\n- t'), 'type', 11, { start: 8, end: 8 });
  assert.equal(h.steps(), 3, 'a script write stands alone and nothing joins it');
  assert.deepEqual(h.undo(), end('- one\n- '), 'the typing goes first');
  assert.deepEqual(h.undo(), end('- one'), 'and then the write itself, whole');
}

{
  // Recording after an undo is a new branch: the redo it would have gone back
  // to is no longer something that happened.
  const h = history('');
  typing(h, ['a']);
  typing(h, ['ab'], 'type', together + 100);
  h.undo();
  assert.equal(h.canRedo(), true, 'the redo is there until something else is written');
  h.record(end('aZ'), 'type', together + 200, { start: 1, end: 1 });
  assert.equal(h.canRedo(), false, 'and gone once it is');
  assert.deepEqual(h.undo(), end('a'), 'the new branch undoes to where it left the old one');
}

{
  // The same text at another caret moves where an undo puts the person back and
  // nothing else. It is what reopening a block records, and a step there would
  // be an undo that appears to do nothing.
  const h = history('one');
  typing(h, ['one two'], 'type', 1);
  h.record({ text: 'one two', start: 0, end: 0 }, 'step', 2);
  assert.equal(h.steps(), 2, 'the same text is not a step');
  h.undo();
  assert.deepEqual(h.redo(), { text: 'one two', start: 0, end: 0 }, 'the caret it carries is the newer one');
}

{
  // The oldest steps go when the cap is reached, so a block somebody has been
  // in all afternoon does not hold the afternoon.
  const over = 50;
  const h = history('0');
  for (let i = 1; i <= cap + over; i++) h.record(end(String(i)), 'step', i);
  assert.equal(h.steps(), cap, 'the list is capped');
  let back = null;
  while (h.canUndo()) back = h.undo();
  assert.equal(back.text, String(over + 1), 'the oldest step left is the oldest that fits under the cap');
}

{
  // reset is somebody else's words arriving. Nothing from before them can be
  // undone to, because every one of those texts is missing what they wrote.
  reset(7, 'ours and theirs');
  const h = of(7, end('ours and theirs'));
  assert.equal(h.canUndo(), false, 'a reset history has nothing behind it');
  typing(h, ['ours and theirs!']);
  assert.deepEqual(h.undo(), end('ours and theirs'), 'and starts again from the text it was reset to');
  reset(7, 'theirs alone');
  assert.equal(of(7, end('theirs alone')).canUndo(), false, 'resetting again drops what was there');
}

{
  // move is the ack of a block this tab drew before the server had one.
  reset(-1, '');
  const h = of(-1, end(''));
  typing(h, ['new']);
  move(-1, 91);
  assert.equal(of(91, end('new')), h, 'the history follows the block to its real id');
  assert.deepEqual(of(91, end('new')).undo(), { text: '', start: 0, end: 0 }, 'holding what was typed under the old one');
  const fresh = of(-1, end(''));
  assert.notEqual(fresh, h, 'and nothing is left behind under it');
}

{
  // keep drops the blocks that are not on the page any more.
  const alive = of(4, end('alive'));
  typing(alive, ['alive!']);
  const gone = of(5, end('gone'));
  typing(gone, ['gone!']);
  keep(new Set([4]));
  assert.equal(of(4, end('alive!')), alive, 'a block still drawn keeps its history');
  assert.equal(of(5, end('gone')).canUndo(), false, 'a block that is gone does not');
}

console.log('undo history cases pass');
