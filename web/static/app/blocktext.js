// What a block's text does to itself, with nothing of the page in it. Nothing
// here touches the DOM or the state, so it can be run under node as well as in
// the browser, which is what web/blocktext_test.mjs does.

// span is the one run of characters that differs between two texts, found by
// the common prefix and the common suffix. Two edits made a second apart are
// one run each, which is what makes the rebase below possible at all: a general
// diff would have to guess which of several runs belongs to whom.
export function span(from, to) {
  const max = Math.min(from.length, to.length);
  let start = 0;
  while (start < max && from[start] === to[start]) start++;
  let tail = 0;
  while (tail < max - start && from[from.length - 1 - tail] === to[to.length - 1 - tail]) tail++;
  return { start, end: from.length - tail, ins: to.slice(start, to.length - tail) };
}

// rebase folds a merge the server made into what the person has typed since the
// save went up. sent is the text that went, acked is what the server put in its
// place, now is what is in the textarea and caret is where they are in it.
//
// The two changes are each one span of the sent text. Spans that do not touch
// are edits to different parts of the block, so both are applied and the caret
// is carried along with them: { text, caret }.
//
// Spans that overlap are two people writing the same words, which nothing here
// can settle honestly, so it answers { conflict: true } and the editor asks the
// person. Keeping what is being typed and sending it again would be worse than
// useless: that send would carry the version the server has just reached, the
// server would see a base it agrees with, and it would write one of the two
// texts over the other in silence.
export function rebase(sent, acked, now, caret) {
  if (sent === acked || acked === now) return { text: now, caret: clamp(caret, now.length) };
  // Nothing was typed while the save was in flight, so what came back is simply
  // what the block says.
  if (sent === now) return { text: acked, caret: clamp(caret, acked.length) };

  const mine = span(sent, now);
  const theirs = span(sent, acked);
  const theirsFirst = theirs.end <= mine.start;
  if (!theirsFirst && mine.end > theirs.start) return { conflict: true };

  const [a, b] = theirsFirst ? [theirs, mine] : [mine, theirs];
  const text = sent.slice(0, a.start) + a.ins + sent.slice(a.end, b.start) + b.ins + sent.slice(b.end);
  // The caret is in the text the person is looking at, so it is carried back to
  // where it sits in what was sent and then moved by whatever the server's span
  // added or took away in front of it. A caret at the end of their own typing
  // stays at the end of it rather than being pushed past a neighbouring insert.
  const at = caret <= mine.start ? caret
    : caret > mine.start + mine.ins.length ? caret - mine.ins.length + (mine.end - mine.start)
      : mine.start;
  const shift = theirs.end <= at ? theirs.ins.length - (theirs.end - theirs.start) : 0;
  return { text, caret: clamp(caret + shift, text.length) };
}

const clamp = (at, length) => Math.max(0, Math.min(at, length));
