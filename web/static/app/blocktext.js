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

// carry moves somebody else's caret through what this tab has typed since the
// text they counted it in went up. sent is that text, now is what is in the
// textarea, and offset is where they said they were standing in sent.
//
// It is the rule rebase uses for somebody else's span against this person's
// caret, with the two the other way round: an edit ending at or before the
// caret moves it by what the edit added or took away, an edit that spans it
// puts it at the end of what was written in its place, and an edit after it
// leaves it where it was. So typing exactly where they are standing puts them
// after what was typed, which is what rebase does with the roles swapped.
export function carry(sent, now, offset) {
  if (sent === now) return clamp(offset, now.length);
  const mine = span(sent, now);
  if (mine.end <= offset) return clamp(offset - (mine.end - mine.start) + mine.ins.length, now.length);
  if (mine.start >= offset) return clamp(offset, now.length);
  return clamp(mine.start + mine.ins.length, now.length);
}

// What a tab tells the others about where it is standing: the block, the
// version the offsets are counted in, and the two ends of the selection. An
// offset only means something against a version, because the text a block
// holds changes under it.
//
// `block:<id>` on its own is the older form and still what an editor says the
// moment it opens: in this block, caret unknown. parseWhere answers that as a
// version of nought, which no block is ever at, so a caller comparing versions
// needs no second question. Anything else, including a card or a document, is
// not about a block and answers null.
export function parseWhere(where) {
  const parts = String(where ?? '').split(':');
  if (parts[0] !== 'block' || (parts.length !== 2 && parts.length !== 5)) return null;
  const block = whole(parts[1]);
  if (!block) return null;
  const here = { block, version: 0, start: 0, end: 0 };
  if (parts.length === 2) return here;
  const [version, start, end] = parts.slice(2).map(whole);
  // A field that is not a number, or a range that reads backwards, is a caret
  // this tab cannot place. The block is still theirs, so it keeps the marker
  // and loses only the caret.
  if (version === null || start === null || end === null || end < start) return here;
  return { block, version, start, end };
}

export function formatWhere(block, version, start, end) {
  return `block:${block}:${version}:${start}:${end}`;
}

// whole is one field of the above: a decimal count and nothing else. Nothing
// here may be negative, and Number('') is nought rather than nothing.
const whole = (s) => (/^\d+$/.test(s) ? Number(s) : null);

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

// A fenced code block opens on up to three spaces and then at least three
// backticks or at least three tildes, and closes on the same character, at
// least as many of them, and nothing but space after them. It is the rule the
// server cuts blocks by, in fence.track in internal/docs/commands.go.
//
// The info string is written as anything but a newline rather than as a dot,
// because a dot does not match U+2028 or U+2029. Those are ordinary characters
// on an ordinary line to the server, and a fence line holding one has to open
// here as well or the two would disagree about where the code is.
const fence = /^ {0,3}(`{3,}|~{3,})([^\n]*)$/;

// step is the fence a line leaves open: the delimiter that opened it, or null
// outside one. A backtick opener's info string holds no backtick, which is what
// keeps a line of inline code from opening a fence that never closes.
function step(open, line) {
  const m = fence.exec(line);
  if (!m) return open;
  const [, marks, info] = m;
  if (open === null) return marks[0] === '`' && info.includes('`') ? null : marks;
  if (marks[0] === open[0] && marks.length >= open.length && !info.trim()) return null;
  return open;
}

// inFence is whether a split between start and end would cut a fenced code
// block in two, which is why Enter in code writes a newline instead. It is two
// questions, one per end of what Enter replaces.
//
// The text in front of start ends with a fence still open: the block above
// would keep a fence nothing closes and the one below would start inside one. A
// caret at the end of the closing fence is out of it and splits as usual, which
// is how somebody writes the paragraph after their code.
//
// And the text in front of end ends with one open, which only differs when
// something is selected: a selection that reaches into a fence takes its
// opening away, and what is left below the cut is the rest of a fence with no
// beginning. Enter over that selection writes a newline into the block the
// person is looking at, where they can see what became of it.
export function inFence(text, start, end = start) {
  const open = (at) => {
    let fenced = null;
    for (const line of text.slice(0, Math.max(0, at)).split('\n')) fenced = step(fenced, line);
    return fenced !== null;
  };
  return open(start) || open(Math.max(start, end));
}

// chunks is what a paste is made of: the paragraphs this editor reads in it,
// blank line separated, with whatever was only whitespace dropped, and blank
// lines inside a fenced code block left where they are. All the editor asks of
// it is whether a paste holds more than one, because the first goes in at the
// caret and the rest go up as one insert for the server to cut up by its own
// rule. So this promises nothing about that rule beyond not handing the server
// half a fence.
export function chunks(text) {
  const out = [];
  let part = [];
  let open = null;
  for (const line of text.replace(/\r\n?/g, '\n').split('\n')) {
    open = step(open, line);
    // Spaces and tabs make a line blank and nothing else does, which is the
    // rule the server cuts paragraphs by. A trim here would also read a line
    // holding a no-break space as the end of one, and the two would disagree
    // about how many paragraphs a paste holds.
    if (open === null && !/[^ \t]/.test(line)) {
      out.push(part.join('\n'));
      part = [];
      continue;
    }
    part.push(line);
  }
  out.push(part.join('\n'));
  return out.filter((each) => each.trim());
}
