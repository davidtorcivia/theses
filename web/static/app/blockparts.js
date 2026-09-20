// What a block's text is made of. parts() answers with the list of things the
// document pane draws, as plain data, and docs.js turns each of them into
// nodes. Nothing here touches the DOM or the state, so it runs under node as
// well as in the browser, which is what web/blocktext_test.mjs does with it.
//
// The pane renders for itself because it has to draw with no server behind it:
// an edit before its ack, an offline document and the snapshot all come through
// here. The export is goldmark, in internal/markdown, and the two agree on
// ordinary text; where they part company the rule below says so.
//
// Every rule below reads forward from a line and answers with the line it
// stopped at, so a block of five thousand headings is five thousand turns of a
// loop rather than five thousand frames on the stack, and nothing in here
// copies the lines it has not read yet. The server takes a block of twenty
// thousand runes, and the pane has to draw whatever it took.

import { step } from './blocktext.js';

export function parts(text) {
  const out = [];
  for (const each of pieces(text)) {
    const lines = each.split('\n');
    for (let at = 0; at < lines.length;) {
      const next = piece(lines, at, out);
      // Every rule below leaves the line it was given behind. One that ever
      // answered with the line it started on would turn this loop for as long
      // as the tab lived, so it says so instead: body() in docs.js catches it
      // and draws the block as the text it holds.
      if (next <= at) throw new Error(`nothing was read at line ${at} of a block`);
      at = next;
    }
  }
  return out;
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

// piece reads one thing off the front of a piece, writes it into out and
// answers with the line after it, which is always further on than the line it
// was given. The order is the order the markers are looked for: a fence first,
// because it holds text that looks like anything else, then the line markers,
// then what is left over as a list or a paragraph.
function piece(lines, at, out) {
  if (step(null, lines[at]) !== null) return code(lines, at, out);
  // A heading is its own line and nothing else, so whatever is under one is
  // read as what it is rather than swept into the heading.
  const hash = /^(#{1,3}) /.exec(lines[at]);
  if (hash) {
    out.push({ kind: 'h' + hash[1].length, text: lines[at].slice(hash[1].length + 1) });
    return at + 1;
  }
  if (/^ {0,3}>/.test(lines[at])) return quote(lines, at, out);
  // A table runs to the end of its piece or to a fence, whichever comes first:
  // a blank line is where the server's markdown ends a table and a piece is
  // what a blank line separates, and a fence ends one as it ends a paragraph.
  const grid = table(lines, at);
  if (grid) {
    out.push(grid);
    return fenceAt(lines, at + 2);
  }

  // Everything else is one list or one paragraph, as far as the fence or the
  // table that ends it: both of those are where the server's markdown ends a
  // paragraph too.
  const fence = fenceAt(lines, at + 1);
  let stop = at + 1;
  while (stop < fence && !table(lines, stop)) stop++;
  const kept = [];
  for (let i = at; i < stop; i++) if (lines[i].trim()) kept.push(lines[i]);
  // Nothing but whitespace is nothing at all. A piece never begins with it, but
  // what is left in front of a fence or a table part way down one can be, and
  // every list rule below reads true of no lines whatever.
  if (!kept.length) return stop;
  if (kept.every((line) => /^- /.test(line))) out.push({ kind: 'ul', items: kept.map((line) => line.slice(2)) });
  else if (kept.every((line) => /^\d+\. /.test(line))) out.push({ kind: 'ol', items: kept.map((line) => line.replace(/^\d+\. /, '')) });
  else out.push({ kind: 'p', text: lines.slice(at, stop).join('\n') });
  return stop;
}

// fenceAt is the first line from here on that opens a fenced code block, or the
// end of the piece. A fence ends whatever stands above it, so a paragraph, a
// list and a table's rows all stop at the same line, and the caller reads the
// code from there.
function fenceAt(lines, from) {
  for (let i = from; i < lines.length; i++) if (step(null, lines[i]) !== null) return i;
  return lines.length;
}

// code is a fenced code block: the lines between the fences as they were typed,
// with the opening fence's own indent taken off each of them the way GitHub
// does it. A fence nothing closes runs to the end of the piece, which is what
// the server's markdown makes of it too. The info string is not kept: nothing
// here highlights code, and the word stays in the markdown a click shows.
function code(lines, at, out) {
  const indent = /^ */.exec(lines[at])[0].length;
  const open = step(null, lines[at]);
  let end = lines.length;
  for (let i = at + 1; i < lines.length; i++) {
    if (step(open, lines[i]) === null) { end = i; break; }
  }
  const text = [];
  for (let i = at + 1; i < end; i++) {
    let cut = 0;
    while (cut < indent && lines[i][cut] === ' ') cut++;
    text.push(lines[i].slice(cut));
  }
  out.push({ kind: 'code', text: text.join('\n') });
  return end + 1;
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
function quote(lines, at, out) {
  let end = at;
  while (end < lines.length && /^ {0,3}>/.test(lines[end])) end++;
  const paragraphs = [];
  let part = [];
  for (let i = at; i < end; i++) {
    const said = lines[i].replace(/^ {0,3}> ?/, '');
    if (said.trim()) part.push(said);
    else if (part.length) { paragraphs.push(part.join('\n')); part = []; }
  }
  if (part.length) paragraphs.push(part.join('\n'));
  out.push({ kind: 'quote', paragraphs });
  return end;
}

// table is a GitHub pipe table starting at a line: a header row, a row of
// dashes under it with one cell per header cell and a colon on the side each
// column is aligned to, then the rows. The row of dashes has to hold a pipe,
// which is what keeps a line of plain dashes under a line of words out of this:
// goldmark reads that as a second level heading and the pane has always drawn
// it as the text it is, and a table of one column would be a third answer and
// the worst of them. A blank header line is no header either, or a row of
// dashes under a blank line would make a table with nothing at the top of it,
// where goldmark leaves the dashes the paragraph they are.
//
// A row short of cells is padded, as goldmark pads it. A row with more cells
// than the header keeps them, where goldmark drops them: nothing in this module
// may leave text somebody typed undrawn, and a cell too many is drawn past the
// last column rather than swallowed.
//
// It is asked at every line of a paragraph, so it answers before it copies
// anything: a line with no pipe under it cannot start one. A fence line never
// passes for a row of dashes, its first cell holding the backticks or the
// tildes it opens with, so the rows below always begin under the header.
function table(lines, at) {
  const rule = lines[at + 1];
  if (rule === undefined || !rule.includes('|') || !lines[at].trim()) return null;
  const head = cells(lines[at]);
  const marks = cells(rule);
  if (!head.length || marks.length !== head.length || !marks.every((c) => /^:?-+:?$/.test(c))) return null;
  const align = marks.map((c) => (c.endsWith(':') ? (c.startsWith(':') ? 'c' : 'r') : (c.startsWith(':') ? 'l' : '')));
  const rows = [];
  const end = fenceAt(lines, at + 2);
  for (let i = at + 2; i < end; i++) {
    if (!lines[i].trim()) continue;
    const row = cells(lines[i]);
    while (row.length < head.length) row.push('');
    rows.push(row);
  }
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
