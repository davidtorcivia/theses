// ponytail: large comparisons show a coarse middle; use a linear-space exact
// diff if aligning unchanged lines inside that middle becomes necessary.
const MAX_CELLS = 250000;

export function diff(a, b) {
  if ((a.length + 1) * (b.length + 1) > MAX_CELLS) return boundedDiff(a, b);

  const lcs = Array.from({ length: a.length + 1 }, () => new Array(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const out = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) { out.push([' ', a[i]]); i++; j++; }
    else if (lcs[i + 1][j] >= lcs[i][j + 1]) { out.push(['-', a[i]]); i++; }
    else { out.push(['+', b[j]]); j++; }
  }
  while (i < a.length) out.push(['-', a[i++]]);
  while (j < b.length) out.push(['+', b[j++]]);
  return out;
}

function boundedDiff(a, b) {
  let head = 0;
  while (head < a.length && head < b.length && a[head] === b[head]) head++;
  let tail = 0;
  while (tail < a.length - head && tail < b.length - head &&
      a[a.length - 1 - tail] === b[b.length - 1 - tail]) tail++;

  const out = a.slice(0, head).map((line) => [' ', line]);
  for (let i = head; i < a.length - tail; i++) out.push(['-', a[i]]);
  for (let i = head; i < b.length - tail; i++) out.push(['+', b[i]]);
  for (let i = a.length - tail; i < a.length; i++) out.push([' ', a[i]]);
  return out;
}
