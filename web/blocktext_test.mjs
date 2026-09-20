// node web/blocktext_test.mjs
//
// The browser modules have no test harness, because everything else about them
// is the page. blocktext.js is the exception: it is arithmetic on two strings
// and a caret, and getting it wrong loses somebody's keystrokes quietly. This
// sits beside the two embedded trees rather than in one of them, so that a test
// is not served to browsers and kept by the service worker.

import assert from 'node:assert/strict';
import { rebase, enter, chunks, carry, inFence, parseWhere, formatWhere } from './static/app/blocktext.js';
import { parts } from './static/app/blockparts.js';

// The caret is written as a pipe in `now` and in `want`, so a case reads as the
// two texts and where the person is standing in them. A case with no `want` is
// one the editor has to ask about.
const at = (text) => [text.replace('|', ''), text.indexOf('|')];

const cases = [
  {
    name: 'the same text back changes nothing',
    sent: 'The tide is high.', acked: 'The tide is high.',
    now: 'The tide is high and|', want: 'The tide is high and|',
  },
  {
    name: 'nothing typed since takes the merged text',
    sent: 'The tide is high.', acked: 'The tide is low.',
    now: 'The tide is high.|', want: 'The tide is low.|',
  },
  {
    name: 'their change before the caret shifts it',
    sent: 'One. Two.', acked: 'One! Two.',
    now: 'One. Two. Three.|', want: 'One! Two. Three.|',
  },
  {
    name: 'their change after the caret leaves it where it was',
    sent: 'One. Two.', acked: 'One. Two!',
    now: 'One, wait.| Two.', want: 'One, wait.| Two!',
  },
  {
    name: 'an insert of theirs at the end of what was typed stays after it',
    sent: 'abc', acked: 'abcZ',
    now: 'aXY|', want: 'aXY|Z',
  },
  {
    name: 'an insert of theirs where the typing starts goes in front of it',
    sent: 'abc', acked: 'aZbc',
    now: 'aXY|', want: 'aZXY|',
  },
  {
    name: 'an insert at nought while they appended',
    sent: 'bc', acked: 'bcZ',
    now: 'a|bc', want: 'a|bcZ',
  },
  {
    name: 'a delete at the end while they wrote at the start',
    sent: 'abc', acked: 'Zabc',
    now: 'ab|', want: 'Zab|',
  },
  {
    name: 'a caret past the end of the text it lands in is pulled back',
    sent: 'One. Two.', acked: 'One.',
    now: 'One. Two.', caret: 40, want: 'One.|',
  },
  // The overlapping rows. Nothing here may guess which of the two texts the
  // person meant, and sending this one again would carry a base the server
  // agrees with and write the other one over without saying so.
  {
    name: 'two changes to the same words are a conflict',
    sent: 'The tide is high.', acked: 'The tide is low.',
    now: 'The tide is slack.|', conflict: true,
  },
  {
    name: 'a delete over the words they rewrote is a conflict',
    sent: 'The tide is high.', acked: 'The tide is low.',
    now: 'The| tide.', conflict: true,
  },
  {
    name: 'typing where they deleted is a conflict',
    sent: 'One two three.', acked: 'One three.',
    now: 'One twenty| three.', conflict: true,
  },
];

for (const c of cases) {
  const [now, caret] = at(c.now);
  const got = rebase(c.sent, c.acked, now, c.caret ?? caret);
  if (c.conflict) {
    assert.deepEqual(got, { conflict: true }, c.name);
    continue;
  }
  const [text, want] = at(c.want);
  assert.deepEqual(got, { text, caret: want }, c.name);
}

console.log(`${cases.length} rebase cases pass`);

// The Enter rows. One pipe is the caret and two are a selection, which is
// replaced before anything else is decided. A case with `want` is one that
// writes the next list item into the same block, caret and all; a case with
// `before` and `after` is a split.
const sel = (text) => {
  const start = text.indexOf('|');
  const second = text.indexOf('|', start + 1);
  return [text.replace(/\|/g, ''), start, second < 0 ? start : second - 1];
};

const enters = [
  { name: 'in the middle of a block', in: 'One.| Two.', before: 'One.', after: ' Two.' },
  { name: 'at the start of a block', in: '|One.', before: '', after: 'One.' },
  { name: 'at the end of a block', in: 'One.|', before: 'One.', after: '' },
  { name: 'in an empty block', in: '|', before: '', after: '' },
  { name: 'a selection goes first', in: 'One |two| three.', before: 'One ', after: ' three.' },
  { name: 'in the middle of a list item stays in the block', in: '- one\n- t|wo', want: '- one\n- t\n- |wo' },
  { name: 'a numbered list numbers the next item', in: '1. one\n2. two|', want: '1. one\n2. two\n3. |' },
  { name: 'at the start of an item the empty one goes above it', in: '- one\n|- two', want: '- one\n- |\n- two' },
  { name: 'at the start of the block the empty item goes above', in: '|- one\n- two', want: '- |\n- one\n- two' },
  { name: 'inside the marker counts as the start of the item', in: '-| two', want: '- |\n- two' },
  { name: 'an empty item above a numbered one keeps its number', in: '1. one\n|2. two', want: '1. one\n2. |\n2. two' },
  { name: 'an empty item at the end leaves the list', in: '- one\n- |', before: '- one', after: '' },
  { name: 'an empty item in the middle splits the list', in: '- one\n- |\n- three', before: '- one', after: '- three' },
];

for (const c of enters) {
  const [text, start, end] = sel(c.in);
  const got = enter(text, start, end);
  if (c.want !== undefined) {
    const [want, caret] = at(c.want);
    assert.deepEqual(got, { kind: 'list', text: want, caret }, c.name);
    continue;
  }
  assert.deepEqual(got, { kind: 'split', before: c.before, after: c.after }, c.name);
}

console.log(`${enters.length} enter cases pass`);

// Where Enter writes a newline rather than cutting the block in two: a caret
// the text in front of which ends inside a fenced code block, or a selection
// with an end like that. One pipe is a caret and two are a selection, as in the
// Enter rows above, and `want` is whether Enter there is a newline. The fence
// rule is the server's, in internal/docs/commands.go, so these read like its
// own cases.
// U+2028 is a line terminator to a regular expression and an ordinary
// character to everything else. It is written as its code point because it
// cannot be seen in the source, and a tool that swallowed it on the way
// through would leave a row that tested nothing.
const lineSeparator = String.fromCharCode(0x2028);

const fences = [
  { name: 'ordinary text is not code', in: 'One.| Two.', want: false },
  { name: 'the end of an opening fence is code', in: '```go|', want: true },
  { name: 'inside a fence is code', in: '```\nA|\nB\n```', want: true },
  { name: 'a blank line inside a fence is code', in: '```\nA\n|\nB\n```', want: true },
  { name: 'the start of the closing fence is code', in: '```\nA\n|```', want: true },
  { name: 'the end of the closing fence is not', in: '```\nA\n```|', want: false },
  { name: 'after a closed fence is not', in: '```\nA\n```\n\nB|', want: false },
  { name: 'in front of an opening fence is not', in: 'One.\n|```\nA\n```', want: false },
  { name: 'a fence that is never closed runs to the end', in: '```\nA\n\nB|', want: true },
  { name: 'a tilde does not close a backtick fence', in: '```\nA\n~~~\nB|', want: true },
  { name: 'a shorter fence does not close a longer one', in: '````\nA\n```\nB|', want: true },
  { name: 'a longer fence does close a shorter one', in: '```\nA\n`````\nB|', want: false },
  { name: 'a fence with words after it does not close', in: '```\nA\n``` and more\nB|', want: true },
  { name: 'four spaces is not a fence', in: '    ```\nA|', want: false },
  { name: 'a backtick in the info string is not a fence', in: '```a``` b\nA|', want: false },
  { name: 'a tilde fence takes an info string with backticks', in: '~~~`\nA|', want: true },
  { name: 'a caret at nought is not in code', in: '|```', want: false },
  { name: 'a closing fence with spaces after it still closes', in: '```\nA\n```  \n\nB|', want: false },
  { name: 'a line separator in the info string does not stop the fence opening', in: '```' + lineSeparator + 'js\nA|', want: true },
  {
    name: 'a selection ending inside a fence is not split',
    in: 'Int|ro line\n```\nA\n|B\n```', want: true,
  },
  {
    name: 'a selection that is nowhere near a fence splits',
    in: 'One |two| three.\n\n```\nA\n```', want: false,
  },
  {
    name: 'a selection ending at the end of the closing fence splits',
    in: 'Intro.\n|```\nA\n```|', want: false,
  },
];

for (const c of fences) {
  const [text, start, end] = sel(c.in);
  assert.equal(inFence(text, start, end), c.want, c.name);
}

console.log(`${fences.length} fence cases pass`);

const pastes = [
  { name: 'one paragraph is one chunk', in: 'One.', want: ['One.'] },
  { name: 'a blank line ends a chunk', in: 'One.\n\nTwo.', want: ['One.', 'Two.'] },
  { name: 'a line of spaces is a blank line', in: 'One.\n \nTwo.', want: ['One.', 'Two.'] },
  { name: 'line endings are normalised', in: 'One.\r\n\r\nTwo.', want: ['One.', 'Two.'] },
  { name: 'what is only whitespace is dropped', in: 'One.\n\n  \n\nTwo.\n\n', want: ['One.', 'Two.'] },
  { name: 'a single newline stays inside its chunk', in: 'One.\nTwo.', want: ['One.\nTwo.'] },
  {
    name: 'a blank line inside a fence stays in its chunk',
    in: '```\nOne.\n\nTwo.\n```', want: ['```\nOne.\n\nTwo.\n```'],
  },
  {
    name: 'a paragraph after a closed fence is its own chunk',
    in: '```\nOne.\n\nTwo.\n```\n\nThree.', want: ['```\nOne.\n\nTwo.\n```', 'Three.'],
  },
];

for (const c of pastes) assert.deepEqual(chunks(c.in), c.want, c.name);

console.log(`${pastes.length} paste cases pass`);

// Somebody else's caret carried through what this tab has typed since the text
// they counted it in went up. The pipe in `sent` is where they said they were,
// and the pipe in `want` is where that lands in `now`.
const carries = [
  { name: 'nothing typed leaves it alone', sent: 'One. |Two.', now: 'One. Two.', want: 'One. |Two.' },
  { name: 'an insert before it moves it', sent: 'One. |Two.', now: 'One! Wait. Two.', want: 'One! Wait. |Two.' },
  { name: 'an insert after it leaves it', sent: 'One. |Two.', now: 'One. Two. Three.', want: 'One. |Two. Three.' },
  { name: 'a delete before it moves it back', sent: 'One. |Two.', now: 'Two.', want: '|Two.' },
  { name: 'an insert exactly where they stand puts them after it', sent: 'One. |Two.', now: 'One. and Two.', want: 'One. and |Two.' },
  { name: 'a replacement spanning them ends at what was written', sent: 'One. T|wo.', now: 'One. Zebra.', want: 'One. Zebra|.' },
  { name: 'a replacement starting where they stand leaves them in front of it', sent: 'One. |Two.', now: 'One. Zebra.', want: 'One. |Zebra.' },
  { name: 'everything taken away puts them at nought', sent: 'One. T|wo.', now: '', want: '|' },
  { name: 'a caret past the end of what was typed is clamped', sent: 'One.|', now: 'On', want: 'On|' },
];

for (const c of carries) {
  const [sent, offset] = at(c.sent);
  const [now, want] = at(c.want);
  assert.equal(now, c.now, c.name + ' (the case itself)');
  assert.equal(carry(sent, now, offset), want, c.name);
}

console.log(`${carries.length} carry cases pass`);

// What a tab says about where it is standing, read back. A string that is not
// about a block at all is nothing to this, and a block with a caret nobody can
// place is still a block somebody is in.
const wheres = [
  { name: 'the older form is a block with no caret', in: 'block:12', want: { block: 12, version: 0, start: 0, end: 0 } },
  { name: 'a caret in a block at a version', in: 'block:12:5:3:3', want: { block: 12, version: 5, start: 3, end: 3 } },
  { name: 'a selection', in: 'block:12:5:3:9', want: { block: 12, version: 5, start: 3, end: 9 } },
  { name: 'a document is not a block', in: 'doc:4', want: null },
  { name: 'a card is not a block', in: 'card:7', want: null },
  { name: 'nothing open', in: '', want: null },
  { name: 'nothing at all', in: null, want: null },
  { name: 'rubbish', in: 'block!12', want: null },
  { name: 'a block that is not a number', in: 'block:x', want: null },
  { name: 'block nought is no block', in: 'block:0', want: null },
  { name: 'a field short', in: 'block:12:5:3', want: null },
  { name: 'a negative offset keeps the block and loses the caret', in: 'block:12:5:-1:3', want: { block: 12, version: 0, start: 0, end: 0 } },
  { name: 'a field that is not a number keeps the block', in: 'block:12:5:x:3', want: { block: 12, version: 0, start: 0, end: 0 } },
  { name: 'an empty field keeps the block', in: 'block:12:5::3', want: { block: 12, version: 0, start: 0, end: 0 } },
  { name: 'a range that reads backwards keeps the block', in: 'block:12:5:9:3', want: { block: 12, version: 0, start: 0, end: 0 } },
];

for (const c of wheres) assert.deepEqual(parseWhere(c.in), c.want, c.name);
assert.equal(formatWhere(12, 5, 3, 9), 'block:12:5:3:9', 'a caret is written as it is read');
assert.deepEqual(parseWhere(formatWhere(12, 5, 3, 9)), { block: 12, version: 5, start: 3, end: 9 },
  'what is written comes back');

console.log(`${wheres.length} where cases pass`);

// What a block's text is made of, which the document pane draws. The first
// eighteen cases, as far as the blank line below them, are what the pane drew
// before any of the four new shapes existed, written down from the old renderer
// so that teaching it the new ones is not allowed to move the old ones; the
// rest are the new ones.
const shapes = [
  { name: 'a paragraph', in: 'One.', want: [{ kind: 'p', text: 'One.' }] },
  { name: 'a first level heading', in: '# Title', want: [{ kind: 'h1', text: 'Title' }] },
  { name: 'a second level heading', in: '## Title', want: [{ kind: 'h2', text: 'Title' }] },
  {
    name: 'a heading is its own line and what is under it is its own piece',
    in: '# Title\nand words',
    want: [{ kind: 'h1', text: 'Title' }, { kind: 'p', text: 'and words' }],
  },
  { name: 'a hash with no space is not a heading', in: '#Title', want: [{ kind: 'p', text: '#Title' }] },
  { name: 'a bullet list', in: '- one\n- two', want: [{ kind: 'ul', items: ['one', 'two'] }] },
  { name: 'a numbered list', in: '1. one\n2. two', want: [{ kind: 'ol', items: ['one', 'two'] }] },
  {
    name: 'one line that is not a bullet makes the whole piece a paragraph',
    in: '- one\nand two',
    want: [{ kind: 'p', text: '- one\nand two' }],
  },
  {
    name: 'a blank line cuts a list in two',
    in: '- one\n\n- two',
    want: [{ kind: 'ul', items: ['one'] }, { kind: 'ul', items: ['two'] }],
  },
  { name: 'a single newline stays in its paragraph', in: 'One.\nTwo.', want: [{ kind: 'p', text: 'One.\nTwo.' }] },
  {
    name: 'a blank line separates two paragraphs',
    in: 'One.\n\nTwo.',
    want: [{ kind: 'p', text: 'One.' }, { kind: 'p', text: 'Two.' }],
  },
  { name: 'a line of spaces is a blank line', in: 'One.\n \nTwo.', want: [{ kind: 'p', text: 'One.' }, { kind: 'p', text: 'Two.' }] },
  {
    name: 'the second of two blank lines in a row belongs to the piece under it',
    in: 'a\n\n\nb',
    want: [{ kind: 'p', text: 'a' }, { kind: 'p', text: '\nb' }],
  },
  {
    name: 'and a heading under two blank lines is still the text it was drawn as',
    in: 'a\n\n\n# b',
    want: [{ kind: 'p', text: 'a' }, { kind: 'p', text: '\n# b' }],
  },
  { name: 'a trailing newline stays on its paragraph', in: 'One.\n', want: [{ kind: 'p', text: 'One.\n' }] },
  { name: 'whitespace is nothing at all', in: '  \n\t\n  ', want: [] },
  { name: 'a pipe in a sentence is not a table', in: 'One | two.', want: [{ kind: 'p', text: 'One | two.' }] },
  { name: 'dashes under words are not a table', in: 'One\n---', want: [{ kind: 'p', text: 'One\n---' }] },

  { name: 'a third level heading', in: '### Title', want: [{ kind: 'h3', text: 'Title' }] },
  { name: 'a fourth level heading is not one', in: '#### Title', want: [{ kind: 'p', text: '#### Title' }] },
  { name: 'a quote', in: '> One.', want: [{ kind: 'quote', paragraphs: ['One.'] }] },
  {
    name: 'consecutive quote lines are one quote of one paragraph',
    in: '> One.\n> Two.',
    want: [{ kind: 'quote', paragraphs: ['One.\nTwo.'] }],
  },
  {
    name: 'a bare marker separates a quote into paragraphs',
    in: '> One.\n>\n> Two.',
    want: [{ kind: 'quote', paragraphs: ['One.', 'Two.'] }],
  },
  {
    name: 'a line with no marker ends the quote',
    in: '> One.\nTwo.',
    want: [{ kind: 'quote', paragraphs: ['One.'] }, { kind: 'p', text: 'Two.' }],
  },
  { name: 'a quote keeps its own inline markup', in: '> *One*.', want: [{ kind: 'quote', paragraphs: ['*One*.'] }] },
  {
    name: 'a fenced code block',
    in: '```\nOne.\n```',
    want: [{ kind: 'code', text: 'One.' }],
  },
  {
    name: 'code keeps its blank lines, its hashes and its spaces',
    in: '```js\nconst a = 1;\n\n# not a heading\n  indented\n```',
    want: [{ kind: 'code', text: 'const a = 1;\n\n# not a heading\n  indented' }],
  },
  {
    name: 'an indented fence takes its own indent off the code',
    in: '  ```\n  One.\nTwo.\n  ```',
    want: [{ kind: 'code', text: 'One.\nTwo.' }],
  },
  {
    name: 'a fence nothing closes runs to the end',
    in: '```\nOne.',
    want: [{ kind: 'code', text: 'One.' }],
  },
  {
    name: 'a fence part way down a piece ends the paragraph above it',
    in: 'One.\n```\ntwo\n```\nThree.',
    want: [{ kind: 'p', text: 'One.' }, { kind: 'code', text: 'two' }, { kind: 'p', text: 'Three.' }],
  },
  {
    name: 'a line of inline code is not a fence',
    in: '```js``` and more',
    want: [{ kind: 'p', text: '```js``` and more' }],
  },
  {
    name: 'a table',
    in: '| a | b |\n| --- | --- |\n| 1 | 2 |',
    want: [{ kind: 'table', align: ['', ''], head: ['a', 'b'], rows: [['1', '2']] }],
  },
  {
    name: 'a table needs no outer pipes and takes its alignment from the colons',
    in: 'a | b | c\n:--- | :---: | ---:\n1 | 2 | 3',
    want: [{ kind: 'table', align: ['l', 'c', 'r'], head: ['a', 'b', 'c'], rows: [['1', '2', '3']] }],
  },
  {
    name: 'an escaped pipe stays in its cell',
    in: '| a | b |\n| - | - |\n| one \\| two | three |',
    want: [{ kind: 'table', align: ['', ''], head: ['a', 'b'], rows: [['one | two', 'three']] }],
  },
  {
    name: 'a short row is padded and a long one keeps the cells it has',
    in: '| a | b |\n| - | - |\n| 1 |\n| 1 | 2 | 3 |',
    want: [{ kind: 'table', align: ['', ''], head: ['a', 'b'], rows: [['1', ''], ['1', '2', '3']] }],
  },
  {
    name: 'a line with more pipes than the header keeps every word of it',
    in: '| a |\n| - |\n| 1 |\n# x | y',
    want: [{ kind: 'table', align: [''], head: ['a'], rows: [['1'], ['# x', 'y']] }],
  },
  {
    name: 'a row of dashes under a blank line is not a table',
    in: 'a\n\n\n|---|\n| 1 |',
    want: [{ kind: 'p', text: 'a' }, { kind: 'p', text: '\n|---|\n| 1 |' }],
  },
  {
    name: 'a delimiter row of the wrong width is not a table',
    in: '| a | b |\n| --- |\n| 1 | 2 |',
    want: [{ kind: 'p', text: '| a | b |\n| --- |\n| 1 | 2 |' }],
  },
  {
    name: 'a delimiter row that is not all dashes is not a table',
    in: '| a | b |\n| --- | x |',
    want: [{ kind: 'p', text: '| a | b |\n| --- | x |' }],
  },
  {
    name: 'a header row needs no pipe of its own, as long as the dashes have one',
    in: 'a\n|---|',
    want: [{ kind: 'table', align: [''], head: ['a'], rows: [] }],
  },
  {
    name: 'a table under a line of prose ends it',
    in: 'Intro\n| a | b |\n| - | - |\n| 1 | 2 |',
    want: [{ kind: 'p', text: 'Intro' }, { kind: 'table', align: ['', ''], head: ['a', 'b'], rows: [['1', '2']] }],
  },
  {
    name: 'a table ends at the blank line that ends its piece',
    in: '| a |\n| - |\n| 1 |\n\nAfter.',
    want: [{ kind: 'table', align: [''], head: ['a'], rows: [['1']] }, { kind: 'p', text: 'After.' }],
  },
  {
    name: 'the blank line a piece starts with is nothing in front of a fence',
    in: 'a\n\n\n```\nx\n```',
    want: [{ kind: 'p', text: 'a' }, { kind: 'code', text: 'x' }],
  },
  {
    name: 'and nothing in front of a table',
    in: 'a\n\n\n| b |\n| - |',
    want: [{ kind: 'p', text: 'a' }, { kind: 'table', align: [''], head: ['b'], rows: [] }],
  },
  {
    name: 'three blank lines in a row are still two paragraphs',
    in: 'a\n\n\n\nb',
    want: [{ kind: 'p', text: 'a' }, { kind: 'p', text: 'b' }],
  },
  {
    name: 'a fence holding a blank line is one piece',
    in: 'One.\n\n```\ntwo\n\nthree\n```\n\nFour.',
    want: [{ kind: 'p', text: 'One.' }, { kind: 'code', text: 'two\n\nthree' }, { kind: 'p', text: 'Four.' }],
  },
];

for (const c of shapes) assert.deepEqual(parts(c.in), c.want, c.name);

console.log(`${shapes.length} shape cases pass`);

// A block is as long as the server lets it be, twenty thousand runes, and the
// pane has to draw whatever is in it. These are the shapes that used to be read
// by one call per line of them: a block of nothing but headings, and one of
// nothing but fences. The document is drawn inside the board's pane, so a
// renderer that ran out of stack over one block would take the proposition off
// the page for everybody who opened it.
//
// The times are a guard against a rule that reads the lines it has not come to
// yet, which is quadratic and was: the bound is forty times the few
// milliseconds each of these takes, so it fails on a mistake in the module
// rather than on a busy machine.
const long = [
  { name: 'five thousand headings', in: '# a\n'.repeat(5000), want: 5000, kind: 'h1' },
  { name: 'five thousand fences', in: '```\n'.repeat(5000), want: 2500, kind: 'code' },
  { name: 'ten thousand lines of prose', in: 'a\n'.repeat(10000), want: 1, kind: 'p' },
  { name: 'ten thousand table rows', in: '| a |\n| - |\n' + '| 1 |\n'.repeat(10000), want: 1, kind: 'table' },
  { name: 'ten thousand quoted lines', in: '> a\n'.repeat(10000), want: 1, kind: 'quote' },
  { name: 'ten thousand lines of code', in: '```\n' + 'a\n'.repeat(10000) + '```', want: 1, kind: 'code' },
];

for (const c of long) {
  const began = performance.now();
  const got = parts(c.in);
  const took = performance.now() - began;
  assert.equal(got.length, c.want, c.name + ': how many parts');
  assert.equal(got[0].kind, c.kind, c.name + ': what they are');
  assert.ok(took < 200, `${c.name}: took ${took.toFixed(1)}ms, which is over the bound`);
}

console.log(`${long.length} long block cases pass`);

// Nothing in the module may leave text somebody typed undrawn. Every line below
// carries a word of its own, and every text that can be built out of three of
// them has to hand all of those words back in the parts it reads. The markers
// themselves are syntax and are not looked for; a fence's info string is syntax
// here too, so the lines below do not put a word in one.
const marked = [
  'wpara', '# whead', '## whead2', '### whead3', '- wbullet', '1. wnumber',
  '> wquote', '>', '```', 'wcode', '| wcellone | wcelltwo |', '| --- | --- |',
  '| wrowone | wrowtwo | wrowthree |', '', '   ', 'a | wpipe', '|---|', 'wsetext', '---',
];

const words = (part) => [part.text, ...(part.items || []), ...(part.paragraphs || []),
  ...(part.head || []), ...(part.rows || []).flat()].filter(Boolean).join(' ');

let texts = 0;
for (const one of marked) {
  for (const two of marked) {
    for (const three of marked) {
      const text = `${one}\n${two}\n${three}`;
      texts++;
      const said = parts(text).map(words).join(' ');
      for (const word of text.match(/w[a-z]+/g) || []) {
        assert.ok(said.includes(word), `${JSON.stringify(text)} lost ${word}: ${JSON.stringify(said)}`);
      }
    }
  }
}

console.log(`${texts} generated texts keep every word`);
