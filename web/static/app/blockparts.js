// What a block's text is made of. parts() answers with the list of things the
// document pane draws, as plain data, and docs.js turns each of them into
// nodes. Nothing here touches the DOM or the state, so it runs under node as
// well as in the browser, which is what web/blocktext_test.mjs does with it.
//
// The pane renders for itself because it has to draw with no server behind it:
// an edit before its ack, an offline document and the snapshot all come through
// here. The export is goldmark, in internal/markdown, and the two agree on
// ordinary text; where they part company the rule below says so.

import { fence, step } from './blocktext.js';

export function parts(text) {
  return pieces(text).flatMap(piece);
}

// pieces is the text cut into the runs a blank line separates, with a fenced
// code block kept whole. Outside a fence it cuts exactly where
// text.split(/\n[ \t]*\n/) cut before, down to the newline a third blank line
// in a row leaves on the front of the piece after it: a separator eats the
// newline in front of the blank line and the one behind it, so the next blank
// line has no newline of its own left to cut with and belongs to the piece it
// starts. Spaces and tabs make a line blank and nothing else does, which is the
// rule the server cuts paragraphs by.
function pieces(text) {
  const lines = text.split('\n');
  const out = [];
  let from = 0;
  let open = null;
  for (let i = 0; i < lines.length; i++) {
    open = step(open, lines[i]);
    if (open === null && i > from && i < lines.length - 1 && !/[^ \t]/.test(lines[i])) {
      out.push(lines.slice(from, i).join('\n'));
      from = i + 1;
    }
  }
  out.push(lines.slice(from).join('\n'));
  return out.filter((each) => each.trim());
}

// piece is one of those read as what it is. The order is the order the markers
// are looked for: a fence first, because it holds text that looks like anything
// else, then the line markers, then the shapes that take more than one line.
function piece(text) {
  // Nothing but whitespace is nothing at all. A piece is never only that, but
  // what is left in front of a fence or a table part way down one can be, and
  // every list rule below reads true of no lines whatever.
  if (!text.trim()) return [];
  const lines = text.split('\n');
  const opens = lines.findIndex((line) => step(null, line) !== null);
  // A fence part way down a piece is where the server's markdown ends the
  // paragraph and starts the code, so it ends this one too.
  if (opens > 0) return [...piece(lines.slice(0, opens).join('\n')), ...piece(lines.slice(opens).join('\n'))];
  if (opens === 0) return code(lines);

  // A heading is its first line and nothing else, so whatever is under one is
  // read as what it is rather than swept into the heading.
  const hash = /^(#{1,3}) /.exec(lines[0]);
  if (hash) return [{ kind: 'h' + hash[1].length, text: lines[0].slice(hash[1].length + 1) }, ...more(lines.slice(1))];
  if (/^ {0,3}>/.test(lines[0])) return quote(lines);
  // A table runs to the end of its piece, because a blank line is where the
  // server's markdown ends one and a piece is what a blank line separates. Like
  // a fence, one part way down a piece ends the paragraph above it.
  for (let i = 0; i < lines.length; i++) {
    const grid = table(lines.slice(i));
    if (!grid) continue;
    return i ? [...piece(lines.slice(0, i).join('\n')), grid] : [grid];
  }

  const kept = lines.filter((line) => line.trim());
  if (kept.every((line) => /^- /.test(line))) return [{ kind: 'ul', items: kept.map((line) => line.slice(2)) }];
  if (kept.every((line) => /^\d+\. /.test(line))) return [{ kind: 'ol', items: kept.map((line) => line.replace(/^\d+\. /, '')) }];
  return [{ kind: 'p', text }];
}

// more is what is left under a heading, a quote or a fence, read as its own
// piece.
const more = (lines) => piece(lines.join('\n'));

// code is a fenced code block: the lines between the fences as they were typed,
// with the opening fence's own indent taken off each of them the way GitHub
// does it. A fence nothing closes runs to the end of the piece, which is what
// the server's markdown makes of it too. The info string is kept as the word it
// starts with, for a reader who wants to know what the code is; nothing here
// highlights it.
function code(lines) {
  const [, marks, info] = fence.exec(lines[0]);
  const indent = lines[0].indexOf(marks[0]);
  const open = step(null, lines[0]);
  let end = lines.length;
  for (let i = 1; i < lines.length; i++) {
    if (step(open, lines[i]) === null) { end = i; break; }
  }
  const text = lines.slice(1, end).map((line) => {
    let cut = 0;
    while (cut < indent && line[cut] === ' ') cut++;
    return line.slice(cut);
  });
  return [{ kind: 'code', lang: info.trim().split(/\s+/)[0], text: text.join('\n') }, ...more(lines.slice(end + 1))];
}

// quote is the run of lines that open with a marker, the marker and one space
// after it taken off. A bare marker separates paragraphs, which is why a quote
// holds paragraphs rather than one text.
//
// A line with no marker ends the quote here and continues its last paragraph in
// goldmark, which calls that a lazy continuation. This is the one place the two
// disagree about text somebody is likely to write, and it is the safe way to be
// wrong: the line is drawn where it was typed rather than pulled into the quote
// above it. Nothing inside a quote nests either: its paragraphs carry inline
// markup and no list, heading or code of their own.
function quote(lines) {
  let end = lines.findIndex((line) => !/^ {0,3}>/.test(line));
  if (end < 0) end = lines.length;
  const paragraphs = [];
  let part = [];
  for (const line of lines.slice(0, end)) {
    const said = line.replace(/^ {0,3}> ?/, '');
    if (said.trim()) part.push(said);
    else if (part.length) { paragraphs.push(part.join('\n')); part = []; }
  }
  if (part.length) paragraphs.push(part.join('\n'));
  return [{ kind: 'quote', paragraphs }, ...more(lines.slice(end))];
}

// table is a GitHub pipe table: a header row, a row of dashes under it with one
// cell per header cell and a colon on the side each column is aligned to, then
// the rows. The row of dashes has to hold a pipe, which is what keeps a line of
// plain dashes under a line of words out of this: goldmark reads that as a
// second level heading and the pane has always drawn it as the text it is, and
// a table of one column would be a third answer and the worst of them. A row
// short of cells is padded and a long one is cut, which is how goldmark keeps
// every row the width of the header.
function table(lines) {
  if (lines.length < 2 || !lines[1].includes('|')) return null;
  const head = cells(lines[0]);
  const rule = cells(lines[1]);
  if (!head.length || rule.length !== head.length || !rule.every((c) => /^:?-+:?$/.test(c))) return null;
  const align = rule.map((c) => (c.endsWith(':') ? (c.startsWith(':') ? 'c' : 'r') : (c.startsWith(':') ? 'l' : '')));
  const rows = lines.slice(2).filter((line) => line.trim()).map((line) => {
    const row = cells(line);
    return head.map((_, i) => row[i] ?? '');
  });
  return { kind: 'table', align, head, rows };
}

// cells splits a row on its pipes. A pipe written as \| is a pipe in the cell
// rather than the end of it, and it is the only escape undone here: everything
// else in a cell is inline markdown and is read as such where it is drawn.
// The pipe that opens a row and the pipe that closes it are punctuation, not
// empty cells on either end.
function cells(row) {
  const text = row.trim();
  const out = [];
  let cell = '';
  let bar = false;
  for (let i = 0; i < text.length; i++) {
    bar = false;
    if (text[i] === '\\' && text[i + 1] === '|') { cell += '|'; i++; continue; }
    if (text[i] === '|') { out.push(cell); cell = ''; bar = true; continue; }
    cell += text[i];
  }
  out.push(cell);
  if (bar) out.pop();
  if (text.startsWith('|')) out.shift();
  return out.map((each) => each.trim());
}
