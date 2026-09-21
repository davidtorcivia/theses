import assert from 'node:assert/strict';
import { inline } from './static/app/dom.js';
import { propositionMatches, referenceKey, referenceIDs, propositionToken } from './static/app/references.js';

globalThis.document = {
  createElement: (tag) => ({ tag, attributes: {}, children: [],
    setAttribute(k, v) { this.attributes[k] = v; },
    append(value) { this.children.push(value); },
  }),
};
const p = { id: 42, number: 7, title: '<Unsafe> title', status: 'Research', kind: 'proposition' };
const lookup = (id) => id === p.id ? p : null;
const render = (text) => inline(text, () => null, lookup);
const [link] = render(propositionToken(42));
assert.equal(link.tag, 'a');
assert.equal(link.textContent, '07 <Unsafe> title');
assert.equal(link.attributes.href, '/p/42');
assert.equal(link.attributes.title, 'Research');
assert.equal(render('@[p:41]')[0].textContent, 'Unavailable proposition');
for (const text of ['@[p:0]', '@[p:-1]', '@[p:9007199254740993]', 'draft@[p:42]', 'draft_@[p:42]', 'é@[p:42]', '`@[p:42]`', '``@[p:42]``']) {
  assert.equal(render(text).join(''), text, text);
}
assert.equal(render('(@[p:42])')[1].attributes.href, '/p/42');
for (const href of ['/p/42', '/show', 'https://example.com']) {
  assert.equal(render(`[Read](${href})`)[0].attributes.href, href);
}
for (const href of ['javascript:alert(1)', '//example.com', '/settings', '/p/0', '/p/42/evil']) {
  assert.equal(render(`[Read](${href})`).join(''), `[Read](${href})`);
}
const key = referenceKey('@[p:42]', lookup);
p.title = 'Renamed';
assert.notEqual(referenceKey('@[p:42]', lookup), key);
assert.equal(render('@[p:42]')[0].textContent, '07 Renamed');
assert.notEqual(referenceKey('@[p:42]', lookup), referenceKey('@[p:42]', () => null));
assert.deepEqual(referenceIDs('@[p:42] @[p:42] @[p:0] @[p:9007199254740993]'), [42]);
assert.deepEqual(propositionMatches([p, { ...p, id: 43, archived_at: 1 }, { ...p, id: 44, kind: 'show' }], 'renamed'), [p]);
assert.deepEqual(propositionMatches([p], '7'), [p]);

assert.equal(render('**@[p:42]**')[0].children[0].attributes.href, '/p/42');
assert.deepEqual(referenceIDs('draft@[p:42] `@[p:43]` @[p:44]'), [44]);

assert.deepEqual(propositionMatches([p], '07'), [p]);
assert.deepEqual(propositionMatches([{ ...p, title: 'Énergie' }], 'éner').map(p => p.id), [42]);
