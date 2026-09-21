import assert from 'node:assert/strict';

globalThis.document = { querySelector: () => null };
globalThis.addEventListener = () => {};
globalThis.requestAnimationFrame = () => 1;
const { boot, state, apply, proposition, user } = await import('./static/app/state.js');
const { referenceKey } = await import('./static/app/references.js');
const card = (id, title, comments = []) => ({ id, title, comments, checklist: [], column_id: 1, position: 'V' });
const event = (seq, entity, id, after, before = null) => ({ seq, proposition: 1, entity, entity_id: id, action: after ? 'edit' : 'delete', after, before });
boot({ me: 1, open: 1, propositions: [{ id: 1, members: [], position: 'V' }], board: { seq: 5, cards: [card(1, 'original'), card(2, 'other')] } });
apply(event(11, 'card', 1, card(1, 'newest')));
apply(event(10, 'card', 1, card(1, 'old')));
assert.equal(state.cards.get(1).title, 'newest', 'late catch-up must not overwrite a newer event');
apply(event(9, 'card', 2, card(2, 'also updated')));
assert.equal(state.cards.get(2).title, 'also updated', 'an older event for another row is still needed');
apply(event(4, 'card', 3, card(3, 'deleted before snapshot')));
assert.equal(state.cards.has(3), false, 'old history cannot resurrect a row absent from the snapshot');
apply(event(12, 'card', 1, null, card(1, 'newest')));
apply(event(11, 'card', 1, card(1, 'newest')));
assert.equal(state.cards.has(1), false, 'late updates cannot resurrect deleted cards');
const note = { id: 7, card_id: 2, body_md: 'new note' };
apply(event(15, 'comment', 7, note));
apply(event(14, 'card', 2, card(2, 'renamed')));
assert.equal(state.cards.get(2).title, 'renamed');
assert.deepEqual(state.cards.get(2).comments, [note], 'a stale parent snapshot must retain a newer child');
apply(event(16, 'comment', 7, null, note));
apply(event(15, 'card', 2, card(2, 'renamed again', [note])));
assert.deepEqual(state.cards.get(2).comments, [], 'parent snapshots must not restore deleted children');
apply(event(18, 'card', 2, card(2, 'latest', [{ ...note, body_md: 'edited' }])));
apply(event(17, 'comment', 7, note));
assert.equal(state.cards.get(2).comments[0].body_md, 'edited', 'late children cannot overwrite newer parent snapshots');
apply(event(0, 'card', 2, card(2, 'optimistic')));
assert.equal(state.cards.get(2).title, 'optimistic', 'local predictions still apply');

const removed = { seq: 1, proposition: 2, entity: 'member', entity_id: 1, action: 'remove',
  after: null, before: { proposition_id: 2, user_id: 1 } };
boot({ me: 1, users: [{ id: 1, role: 'editor' }], open: 1,
  propositions: [{ id: 1, members: [1], position: 'V' }, { id: 2, members: [1], position: 'W' }],
  board: { seq: 0, cards: [] } });
apply(removed);
assert.equal(proposition(2), null, 'self-removal drops the unreadable proposition from client state');
assert.equal(referenceKey('@[p:2]', proposition), '[[2]]', 'references cannot resolve revoked metadata');

boot({ me: 1, users: [{ id: 1, role: 'owner' }], open: 1,
  propositions: [{ id: 1, members: [1], position: 'V' }, { id: 2, members: [1], position: 'W' }],
  board: { seq: 0, cards: [] } });
apply(removed);
assert.ok(proposition(2), 'an owner retains the proposition after membership removal');
assert.equal(proposition(2).members.includes(user(1).id), false, 'the owner membership row is still removed');
console.log('state event ordering cases pass');

const { material } = await import('./static/app/state.js');
Object.defineProperty(globalThis, 'navigator', { value: { onLine: true }, configurable: true });
let release;
const delayed = new Promise((resolve) => { release = resolve; });
let reads = 0;
globalThis.fetch = async () => {
  reads++;
  await delayed;
  return { status: 200, ok: true, headers: new Headers({'content-type':'application/json'}), json: async () => ({ files: [{id: 8}], links: [], attachments: [] }) };
};
state.loaded = 0;
const one = material();
const two = material();
assert.equal(one, two, 'concurrent material callers must wait for the same read');
assert.equal(state.loaded, 0, 'loading is not loaded');
release();
await two;
assert.equal(reads, 3);
assert.equal(state.files[0].id, 8);
assert.equal(state.loaded, state.open);
