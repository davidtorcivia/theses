import assert from 'node:assert/strict';
globalThis.document = { querySelector: () => null };
globalThis.addEventListener = () => {};
globalThis.requestAnimationFrame = () => 1;
globalThis.BroadcastChannel = undefined;
globalThis.navigator ||= {};
Object.defineProperty(globalThis.navigator, 'onLine', { value: true, configurable: true });
globalThis.location = { href: 'https://example.com/p/1', origin: 'https://example.com' };
const { state, boot, apply } = await import('./static/app/state.js');
const { catchUp, fallbackOnce, send } = await import('./static/app/net.js');
const { sessionEnded } = await import('./static/app/response.js');
boot({ me: 1, open: 1, propositions: [{ id: 1, position: 'V' }], board: { seq: 5, cards: [] } });
const event = (seq, id = seq, title = String(seq)) => ({ seq, proposition: 1, entity: 'card', entity_id: id, action: 'create', after: { id, title } });
const cursors = [];
globalThis.fetch = async (url) => {
  const since = Number(new URL(url, 'https://example.com').searchParams.get('since'));
  cursors.push(since);
  if (since === 5) {
    apply(event(450, 6, 'newest'));
    return { ok: true, json: async () => ({ events: Array.from({ length: 200 }, (_, i) => event(i + 6)) }) };
  }
  assert.equal(since, 205, 'pagination follows the HTTP page, not a newer socket event');
  return { ok: true, json: async () => ({ events: [event(206)] }) };
};
assert.equal(await catchUp(), true);
assert.deepEqual(cursors, [5, 205]);
assert.equal(state.cards.size, 201);
assert.equal(state.cards.get(6).title, 'newest');
let release;
globalThis.fetch = async () => new Promise((resolve) => { release = resolve; });
const delayed = catchUp();
globalThis.fetch = async () => ({ ok: true, json: async () => ({ events: [event(451, 6, 'later')] }) });
assert.equal(await catchUp(), true);
release({ ok: true, json: async () => ({ events: [event(450, 6, 'old replay')] }) });
assert.equal(await delayed, true);
assert.equal(state.cards.get(6).title, 'later', 'overlapping reads cannot roll state back');

const response = (body) => ({
  ok: true, status: 200, url: 'https://example.com/app/commands',
  headers: { get: () => 'application/json' },
  json: async () => body,
});
globalThis.fetch = async (url, options = {}) => {
  if (String(url).startsWith('/api/events')) {
    assert.equal(new URL(url, 'https://example.com').searchParams.get('wait'), '0', 'the first fallback probe does not long-poll');
    return response({ events: [] });
  }
  assert.equal(url, '/app/commands');
  const command = JSON.parse(options.body);
  assert.equal(command.cmd, 'card.title');
  assert.ok(command.key, 'HTTP commands keep their idempotency key');
  return response({ type: 'ack', id: command.id, event: event(452, 6, command.args.title) });
};
assert.equal(await fallbackOnce(), true, 'a successful event poll enables HTTP transport');
assert.equal(state.connected, true);
await new Promise((resolve) => setTimeout(resolve, 0));
await send('card.title', { card: 6, base: 0, title: 'over HTTP' });
assert.equal(state.cards.get(6).title, 'over HTTP', 'a command is applied through the HTTP fallback');

boot({ me: 1, open: 0, propositions: [], board: { seq: 0, cards: [] } });
globalThis.fetch = async (url, options = {}) => {
  if (url === '/app/search?q=&limit=1') return response({ query: '', groups: [] });
  assert.equal(url, '/app/commands');
  const command = JSON.parse(options.body);
  assert.equal(command.cmd, 'proposition.create');
  return response({ type: 'ack', id: command.id,
    event: { seq: 453, proposition: 2, entity: 'proposition', entity_id: 2,
      action: 'create', after: { id: 2, title: command.args.title, position: 'V', members: [1] } } });
};
assert.equal(await fallbackOnce(), true, 'an authenticated read enables fallback in an empty workspace');
await send('proposition.create', { title: 'First proposition' });
assert.equal(state.props[0].title, 'First proposition');

const login = {
  ok: true, status: 200, url: 'https://example.com/login',
  headers: { get: () => 'text/html; charset=utf-8' },
};
assert.equal(sessionEnded(login, location.href), true, 'only the same-origin login page means the session ended');
assert.equal(sessionEnded({ ...login, url: 'https://captive.example/login' }, location.href), false,
  'an unrelated login page does not discard cached work');
boot({ me: 1, open: 1, propositions: [{ id: 1, position: 'V' }], board: { seq: 0, cards: [] } });
globalThis.fetch = async () => login;
assert.equal(await catchUp(), false, 'an expired poll does not become a live fallback');
assert.equal(location.href, '/login', 'an expired poll clears the device state and returns to sign in');

console.log('stream pagination and concurrent catch-up cases pass');
