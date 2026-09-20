// node web/blocktext_test.mjs
//
// The browser modules have no test harness, because everything else about them
// is the page. blocktext.js is the exception: it is arithmetic on two strings
// and a caret, and getting it wrong loses somebody's keystrokes quietly. This
// sits beside the two embedded trees rather than in one of them, so that a test
// is not served to browsers and kept by the service worker.

import assert from 'node:assert/strict';
import { rebase } from './static/app/blocktext.js';

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
