// node web/dom_test.mjs
//
// children is the other piece of the browser code worth testing away from a
// page: what it promises is not what the list looks like afterwards, which is
// easy to see, but which nodes it did not touch on the way, which is not. A
// card a finger is holding that leaves the page for an instant loses the
// finger, so the count of how often each node was moved or removed is the
// thing under test. This sits beside blocktext_test.mjs and out of the two
// embedded trees, so that a test is not served to browsers.

import assert from 'node:assert/strict';
import { children } from './static/app/dom.js';

// Enough of a node for children: an ordered list of kids, the two links it
// walks, and the two methods it calls, each counting what it did to a node.
let moved = new Map();

function node(name) {
  return {
    name,
    kids: [],
    parentNode: null,
    get firstChild() { return this.kids[0] || null; },
    get nextSibling() {
      const at = this.parentNode ? this.parentNode.kids.indexOf(this) : -1;
      return at < 0 ? null : this.parentNode.kids[at + 1] || null;
    },
    insertBefore(kid, ref) {
      count(kid, 'insert');
      if (kid.parentNode) kid.parentNode.kids.splice(kid.parentNode.kids.indexOf(kid), 1);
      kid.parentNode = this;
      const at = ref ? this.kids.indexOf(ref) : this.kids.length;
      this.kids.splice(at < 0 ? this.kids.length : at, 0, kid);
      return kid;
    },
    removeChild(kid) {
      count(kid, 'remove');
      this.kids.splice(this.kids.indexOf(kid), 1);
      kid.parentNode = null;
      return kid;
    },
  };
}

function count(kid, what) {
  const was = moved.get(kid.name) || { insert: 0, remove: 0 };
  was[what] += 1;
  moved.set(kid.name, was);
}

function parent(name, kids) {
  const p = node(name);
  for (const kid of kids) p.insertBefore(kid, null);
  return p;
}

const names = (p) => p.kids.map((k) => k.name).join(',');
const touched = (name) => {
  const was = moved.get(name) || { insert: 0, remove: 0 };
  return was.insert + was.remove;
};

// Each case builds a parent out of `from`, asks for `want`, and names the nodes
// that must not have been touched getting there.
const cases = [
  {
    name: 'a head replaced leaves the pane where it is',
    from: ['head', 'pane'], want: ['head2', 'pane'], untouched: ['pane'],
  },
  {
    name: 'a card inserted at the head leaves the rest alone',
    from: ['a', 'b'], want: ['new', 'a', 'b'], untouched: ['a', 'b'],
  },
  {
    name: 'a card removed from the head leaves the rest alone',
    from: ['gone', 'a', 'b'], want: ['a', 'b'], untouched: ['a', 'b'],
  },
  {
    name: 'a card removed from the middle leaves the rest alone',
    from: ['a', 'gone', 'b'], want: ['a', 'b'], untouched: ['a', 'b'],
  },
  {
    name: 'the last card moved to the head moves only itself',
    from: ['a', 'b', 'c'], want: ['c', 'a', 'b'], untouched: ['a', 'b'],
  },
  {
    name: 'a list that has not changed is not touched at all',
    from: ['a', 'b', 'c'], want: ['a', 'b', 'c'], untouched: ['a', 'b', 'c'],
  },
  {
    name: 'an empty list empties the parent',
    from: ['a', 'b'], want: [], untouched: [],
  },
];

for (const c of cases) {
  moved = new Map();
  const made = new Map();
  const of = (name) => made.get(name) || made.set(name, node(name)).get(name);
  const p = parent('p', c.from.map(of));
  moved = new Map();
  children(p, c.want.map(of));
  assert.equal(names(p), c.want.join(','), c.name);
  for (const name of c.untouched) assert.equal(touched(name), 0, `${c.name}: ${name} was touched`);
}

// A card that moved to another column is wanted by a list that has not been
// reconciled yet. Its old column is swept first, and keep is what stops that
// sweep taking it out of the page: it is moved into its new column and never
// removed from anything.
{
  moved = new Map();
  const a = node('a'), k = node('k'), b = node('b');
  const from = parent('from', [a, k]);
  const to = parent('to', [b]);
  moved = new Map();
  const keep = new Set([k]);
  children(from, [a], keep);
  assert.equal(names(from), 'a,k', 'the kept node waits in its old list');
  assert.equal(touched('k'), 0, 'the kept node is not touched by the sweep');
  children(to, [b, k], keep);
  assert.equal(names(from), 'a', 'the kept node has left its old list');
  assert.equal(names(to), 'b,k', 'and is in the list that wanted it');
  assert.equal((moved.get('k') || {}).remove || 0, 0, 'the kept node was never removed');
  assert.equal(touched('b'), 0, 'the list it arrived in was not disturbed');
}

// The falsy an absent child is written as, which is how everything else here
// writes one, is dropped rather than inserted.
{
  moved = new Map();
  const a = node('a');
  const p = parent('p', []);
  children(p, [null, a, false, undefined, 0, '']);
  assert.equal(names(p), 'a', 'nothing but nodes reaches the parent');
}

// Nested lists, because the callers hand over a list of lists.
{
  moved = new Map();
  const a = node('a'), b = node('b'), c = node('c');
  const p = parent('p', []);
  children(p, [a, [b, c], false]);
  assert.equal(names(p), 'a,b,c', 'a list of lists is flattened');
}

console.log(`${cases.length + 3} children cases pass`);
