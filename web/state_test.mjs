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

const block={id:50,document_id:20,text:'restored',version:4,position:'V'};
apply({...event(30,'document',20,{id:20,position:'V',blocks:[block]}),action:'restore'});
apply(event(29,'block',50,{...block,text:'stale',version:3}));
assert.equal(state.documents.find(d=>d.id===20).blocks[0].text,'restored');
apply(event(32,'block',50,{...block,text:'newer',version:5}));
apply({...event(31,'document',20,{id:20,position:'V',blocks:[block]}),action:'restore'});
assert.equal(state.documents.find(d=>d.id===20).blocks[0].text,'newer');
apply(event(34,'document',20,{id:20,title:'Renamed',position:'V'}));
apply(event(33,'block',50,{...block,text:'edit before rename',version:6}));
assert.equal(state.documents.find(d=>d.id===20).blocks[0].text,'edit before rename','row-only rename does not mask block edits');

const priorResearch=state.researchRevision;
apply({...event(35,'evidence',2,{id:2,title:'restored source'}),action:'restore'});
assert.ok(state.researchRevision>priorResearch);
let readsAfterRestore=0;
globalThis.fetch=async path=>{readsAfterRestore++;return new Response(JSON.stringify(path.includes('attachments')?{links:[{card_id:2,link_id:9}]}:{links:[],files:[]}),{headers:{'content-type':'application/json'}});};
apply({...event(36,'link',9,{id:9,proposition_id:1,url:'https://example.com'}),action:'restore'});
await material();
assert.equal(readsAfterRestore,3);
assert.deepEqual(state.attachments.links,[{card_id:2,link_id:9}]);

const {acceptFile}=await import('./static/app/state.js');
apply(event(40,'file',8,{id:8,proposition_id:1,name:'newest'}));
acceptFile({id:8,proposition_id:1,name:'old response'},39);
assert.equal(state.files.find(f=>f.id===8).name,'newest');
apply(event(41,'file',8,null,{id:8,proposition_id:1}));
acceptFile({id:8,proposition_id:1,name:'deleted during read'},40);
assert.equal(state.files.some(f=>f.id===8),false);
acceptFile({id:9,proposition_id:2,name:'other workspace'},41);
assert.equal(state.files.some(f=>f.proposition_id===2),false);
