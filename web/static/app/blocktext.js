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

const bullet = /^- /;
const numbered = /^(\d+)\. /;

// enter is what the Enter key does where the caret is, decided from the text
// alone. A selection is replaced first, so every answer below is about the
// block as it stands with the selected run gone.
//
// Outside a list it is a split: this block keeps `before` and a new one holds
// `after`. On an item of a list, which is a block whose every line carries a
// marker, it is the next item written into the same block, because a list is
// one block and the editor never asks the server to cut one up. An empty item
// is how somebody leaves a list: the marker goes with the newline in front of
// it and the block splits where it stood. At or inside the marker of an item
// that has words in it, the new item goes above rather than cutting the marker
// in half, which is the one place the caret is not where the text is split.
export function enter(text, start, end) {
  const before = text.slice(0, start);
  const after = text.slice(end);
  const now = before + after;
  const caret = before.length;
  const lines = now.split('\n');
  const dashes = lines.every((line) => bullet.test(line));
  const numbers = !dashes && lines.every((line) => numbered.test(line));
  if (!dashes && !numbers) return { kind: 'split', before, after };

  const from = now.lastIndexOf('\n', caret - 1) + 1;
  const stop = now.indexOf('\n', caret);
  const to = stop < 0 ? now.length : stop;
  const line = now.slice(from, to);
  const mark = dashes ? '- ' : line.match(numbered)[0];
  if (line.length === mark.length) {
    return {
      kind: 'split',
      before: now.slice(0, from ? from - 1 : 0),
      after: now.slice(Math.min(to + 1, now.length)),
    };
  }
  // Nothing is renumbered anywhere below: what the list becomes is drawn from
  // the markers as they read, and the item being typed is the only one the
  // person is looking at.
  //
  // A caret in the marker is a caret at the front of the words, so the empty
  // item is made above them and they stay whole. This item's own marker is
  // reused, because the number that follows it is the one this item had.
  if (caret <= from + mark.length) {
    return { kind: 'list', text: now.slice(0, from) + mark + '\n' + now.slice(from), caret: from + mark.length };
  }
  const next = dashes ? '\n- ' : '\n' + (Number(line.match(numbered)[1]) + 1) + '. ';
  return { kind: 'list', text: now.slice(0, caret) + next + now.slice(caret), caret: caret + next.length };
}

// chunks is what a paste is made of: the paragraphs this editor reads in it,
// blank line separated, with whatever was only whitespace dropped. All the
// editor asks of it is whether a paste holds more than one, because the first
// goes in at the caret and the rest go up as one insert for the server to cut
// up by its own rule. So this promises nothing about that rule and does not
// have to agree with it.
export function chunks(text) {
  return text.replace(/\r\n?/g, '\n').split(/\n[ \t]*\n/).filter((part) => part.trim());
}
