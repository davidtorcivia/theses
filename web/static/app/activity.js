// The activity panel: what has happened on this proposition, newest first, with
// undo on the rows core allows it on, and above them the changes this device
// made offline that the server would not take.
//
// It lives in the drawer the cards and the links use, because it is the same
// thing: one column beside the work, closed with Escape.

import { $, el, initials, say } from './dom.js';
import { state, user, emit, canEdit, unresolvedCard } from './state.js';
import { send, again, letGo, resend, Conflict } from './net.js';
import { gone } from './docs.js';
import { modal } from './workflow.js';
import { clear } from './dom.js';
import { apply } from './state.js';
import * as api from './api.js';

export function openPanel() {
  if (unresolvedCard(state.openCard)) {
    say('Choose keep mine or take theirs before opening activity.');
    return false;
  }
  if (state.openCard || state.openLink || state.openFile) history.replaceState(null, '', '#activity');
  state.panel = true;
  state.openCard = state.openLink = state.openFile = null;
  seen = -1; expanded = false;
  emit();
  return true;
}

export function closePanel() {
  const returnFocus = state.panel && $('#drawer')?.contains(document.activeElement);
  clearTimeout(timer); timer = 0;
  state.panel = false;
  // The settings page's Activity tab links to this hash. Left on the address
  // after the panel has been closed, pressing that tab again is a link to the
  // page it is already on and nothing happens at all.
  if (location.hash === '#activity') {
    history.replaceState(null, '', location.pathname + location.search);
  }
  emit();
  if (returnFocus) requestAnimationFrame(() => $('#activitytab')?.focus());
}

// seen is the stream position the list was read at. Every applied event moves
// the state on, so the panel notices it is behind and reads again, which is
// also how an undo row appears without this having to guess what undo did.
let seen = -1;
let timer = 0;
let filter = '', before = 0, more = false, expanded = false, loading = false, failure = '';
let generation = 0;

function refresh() {
  if (seen === state.seq || timer || loading || !state.open || expanded) return;
  timer = setTimeout(() => { timer = 0; readActivity(false); }, 300);
}

async function readActivity(older) {
  if (older && loading) return;
  clearTimeout(timer); timer = 0;
  if (!older) { before = 0; more = false; }
  const request = ++generation;
  const at = state.seq, proposition = state.open;
  loading = true; failure = '';
  emit();
  try {
    const body = await api.get('/activity?proposition=' + proposition + '&entity=' + encodeURIComponent(filter) + (older ? '&before=' + before : ''));
    if (request !== generation || proposition !== state.open) return;
    const rows = body.activity || [];
    state.activity = older ? [...state.activity, ...rows.filter((r) => !state.activity.some((old) => old.seq === r.seq))] : rows;
    before = body.before || 0; more = Boolean(body.more);
    expanded = older;
    if (!older) seen = at;
  } catch {
    if (request !== generation) return;
    failure = 'Could not load activity. Retry when connected.';
    seen = at;
  } finally {
    if (request === generation) { loading = false; emit(); }
  }
}

export function renderPanel(drawer) {
  refresh();
  drawer.append(el('div', { class: 'dh' },
    el('span', { class: 'mono', text: 'Activity' }),
    el('button', { class: 'x', type: 'button', text: 'Close', onclick: closePanel })));

  if (state.refused.length) {
    drawer.append(el('h4', { text: 'Not taken' }));
    const list = el('ul', { class: 'linked' });
    for (const row of state.refused) list.append(refusedRow(row));
    drawer.append(list);
  }

  const select = el('select', { 'aria-label': 'Activity type', 'data-k': 'activity-filter', onchange: (e) => {
    filter = e.target.value; expanded = false; state.activity = []; readActivity(false);
  } }, ['', 'card', 'block', 'document', 'file', 'link', 'comment', 'proposition'].map((kind) =>
    el('option', { value: kind, selected: filter === kind, text: kind || 'All activity' })));
  drawer.append(el('div',{class:'activity-controls'},select,el('button',{type:'button',class:'lnk',text:'Recently deleted',onclick:openTrash})));
  if (failure) drawer.append(el('p', { role: 'status', text: failure }), el('button', { class: 'lnk', type: 'button', text: 'Retry', 'data-k': 'activity-retry', onclick: () => readActivity(false) }));
  if (expanded && seen !== state.seq) drawer.append(el('button', { class: 'lnk', type: 'button', text: 'Load new activity', 'data-k': 'activity-new', onclick: () => readActivity(false) }));
  drawer.append(el('h4', { text: loading ? 'Loading…' : 'History' }));
  const list = el('ol', { class: 'comments' });
  if (!state.activity.length) {
    list.append(el('li', { class: 'dim', text: state.fromCache
      ? 'The log is read from the server. It is here when you are back online.'
      : 'Nothing yet.' }));
  }
  // The oldest group is drawn without the run undo, because the read is one
  // page of the log and a run reaching the bottom of it may carry on below:
  // the oldest row here would then be the middle of a run rather than its
  // start, and taking it back would restore a text from the middle of somebody
  // typing. It costs the bottom line of the panel its control and nothing else.
  const groups = grouped(state.activity);
  groups.forEach((group, i) => list.append(activityRow(group, more && i === groups.length - 1)));
  drawer.append(list);
  if (more) drawer.append(el('button', { class: 'lnk', type: 'button', text: loading ? 'Loading…' : 'Load older activity', 'data-k': 'activity-more', disabled: loading,
    onclick: () => readActivity(true) }));
}

// grouped folds a run of saves by one person on one block into a single line.
// A document saves itself as it is typed, so every block someone writes leaves
// a row a second, and listing all of them would make the panel a typing log
// with the rest of the work scrolled off the bottom of it.
function grouped(rows) {
  const out = [];
  for (const row of rows) {
    const last = out[out.length - 1];
    if (last && follows(last[0], row)) last.push(row);
    else out.push([row]);
  }
  return out;
}

// sitting is how long a run may span, measured from its newest row, because
// that is the row each next one is asked about. A document saves itself every
// few hundred milliseconds while somebody types, so minutes between two saves
// is them coming back to the block rather than still being in it; without this
// one line on the panel would stand for an afternoon and offer to take the
// whole afternoon back in one press.
const sitting = 120;

// via is part of who, not only kind and id. An agent writing through a token
// or MCP is attributed to the person who owns it, so without this their own
// typing and their agent's edits would read as one run and the undo below
// would take the agent's work back as though it were one of their saves.
// core.Compact keys a run the same way, on kind, id and via.
const follows = (a, b) => a.entity === 'block' && a.action === 'set'
  && b.entity === 'block' && b.action === 'set' && a.entity_id === b.entity_id
  && Boolean(a.actor) && Boolean(b.actor)
  && a.actor.kind === b.actor.kind && a.actor.id === b.actor.id
  && (a.actor.via || '') === (b.actor.via || '')
  && a.at - b.at <= sitting;

// A group is drawn at the newest of its rows, which is where its time comes
// from and what its text says. oldest says this is the last group on the page,
// whose run may not be a whole one.
function activityRow(group, oldest) {
  const row = group[0];
  const who = row.actor && row.actor.id ? user(row.actor.id) : { name: row.actor ? row.actor.name : '', initials: '··', colour: 'c8' };
  const via=row.actor?.via||'';
  const attribution=who.name+(via.startsWith('token:')?' via API · '+via.slice(6):via.startsWith('mcp:')?' via MCP · '+via.slice(4):via?' via '+via:'');
  const line = el('div', {},
    el('span',{class:'activity-author',text:attribution}),
    el('p', { class: row.undone ? 'dim' : '', text: describe(row) }),
    el('span', { class: 'mono when', text: when(row.at)
      + (group.length > 1 ? ` · ${group.length} saves` : '')
      + (row.undone ? ' · undone' : '') }));
  const back = canEdit()
    ? (group.length > 1 ? (oldest ? null : takeRunBack(group)) : takeRowBack(row))
    : null;
  if (back) line.append(' ', back);
  return el('li', {}, initials(who), line);
}

// takeRowBack is core's undo of one row: its before put back, and the row
// marked undone so nobody puts it back twice.
function takeRowBack(row) {
  if (!row.undoable) return null;
  return el('button', {
    class: 'lnk quiet', type: 'button', text: 'undo',
    onclick: (e) => {
      const button = e.currentTarget;
      button.disabled = true;
      send('undo', { activity: row.seq }).catch((err) => {
        say(err.message);
        button.disabled = false;
      });
    },
  });
}

// takeRunBack is the same offer over a run of saves, which core's undo cannot
// make: that one puts one row's before back and refuses a row the entity has
// moved past since, so the only row of a run it would take is the newest, which
// is a second of typing rather than the change the line describes.
//
// A run is taken back as an ordinary edit instead: the text the block held
// before the oldest row, sent with the version it reached after the newest. The
// server merges a set whose base is an older version like any other, so
// whatever anybody did to another part of the block after the run is kept and
// only this run's own words go. Nothing here is marked undone, because nothing
// was undone: this is a new change that happens to restore old text, and the
// log says exactly that, which is also what makes it undoable in its turn.
//
// Nothing is offered when the oldest row has no before to go back to, when the
// run left the block reading what it read before it, or when any row of it has
// already been undone on its own: the block then holds the text from before
// that row, and a set based on the version after it is a revert against a
// revert, which is the overlap no merge can make honestly.
//
// ponytail: this reads the run's own two ends and never the block, which
// leaves two rough edges. A run taken back within the sitting it was typed in
// folds the set this sends back into itself, and that line is then offered
// nothing at all, so the redo is only there for a run older than one sitting.
// And pressing the control again on a run already taken back sends a set that
// merges to the text the block already holds, which changes nothing and leaves
// one more row in the log. The upgrade for both is to ask the open document
// what the block reads now rather than what the run left it reading.
function takeRunBack(group) {
  const last = group[0];
  const first = group[group.length - 1];
  const was = first.before && first.before.text;
  const now = last.after && last.after.text;
  if (typeof was !== 'string' || !last.after || !last.after.version) return null;
  if (was === now || group.some((r) => r.undone)) return null;
  return el('button', {
    class: 'lnk quiet', type: 'button', text: 'undo',
    onclick: (e) => {
      const button = e.currentTarget;
      button.disabled = true;
      // whole, because that text was stored exactly as it was typed once
      // already, edges and blank lines included, and putting it back is putting
      // back what was there rather than writing something new.
      //
      // It queues under no name, so with no connection it waits behind a save
      // of the same block rather than folding over it: this is older text
      // against an older base, and a fold keeps whichever came last. Behind it,
      // the two go up in order and the server merges or refuses this one like
      // any other set made from a version somebody has moved past.
      send('block.set', { block: last.entity_id, base: last.after.version, text: was, whole: true },
        state.open, { fold: '' })
        .catch((err) => {
          say(err instanceof Conflict
            ? 'That block has changed too much since for those saves to be taken back.'
            : err.message);
          button.disabled = false;
        });
    },
  });
}

// describe is one line of English for one applied command. The log holds the
// row either side of the change, so the name of the thing is in the payload
// rather than in a table of phrasings here.
function describe(row) {
  const subject = row.after || row.before || {};
  const name = subject.title || subject.name || subject.text || subject.url || '';
  const what = `${row.action} ${row.entity.replace(/_/g, ' ')}`;
  return name ? `${what} · ${clip(name)}` : what;
}

const clip = (text) => (text.length > 80 ? text.slice(0, 79) + '…' : text);

// refusedRow is one command the server would not take when it went up. It shows
// the three texts a merge is about, which is why the outbox keeps the one the
// editor started from: without it a choice made an hour later is made blind.
function refusedRow(row) {
  const li = el('li', {});
  // A save whose block has been deleted since has nowhere to go back to, so the
  // only honest answer is to say so and keep the words here to be copied out of
  // until somebody lets them go. Sending it again would be refused, and this
  // row is the last place what they wrote still exists.
  const lost = row.cmd === 'block.set' && gone(row.args.block);
  const detail = lost ? null : row.detail;
  const mine = row.args.text ?? row.args.title ?? '';
  li.append(el('p', { text: `${row.cmd} was not taken: ${row.refused}` }));
  if (lost) li.append(el('p', { class: 'dim', text: 'That block has since been deleted.' }));
  if (row.base_text) {
    li.append(el('p', { class: 'mono dim', text: 'You started from: ' + clip(row.base_text) }));
  }
  // Clipped, except when this row is the last copy there is. A block somebody
  // deleted takes the save that was refused with it, so the words in the row
  // are the only ones left and the person is being asked to let them go: they
  // have to be able to read all of them and take them out of here first.
  if (mine && lost) li.append(whole(String(mine)));
  else if (mine) li.append(el('p', { class: 'mono dim', text: 'Yours: ' + clip(String(mine)) }));
  if (detail) {
    li.append(el('p', { class: 'mono dim', text: 'Theirs: ' + clip(detail.current || '(nothing)') }));
    li.append(el('button', {
      class: 'lnk', type: 'button', text: 'Keep mine',
      onclick: () => resolve(row, { ...row.args, base: detail.version }),
    }), ' ');
  } else if (!lost) {
    // A refusal with nothing to compare is the server saying no rather than
    // somebody else saying something different, and some of those are worth one
    // more go: a moment when it was busy, a fault it has since recovered from.
    li.append(el('button', { class: 'lnk', type: 'button', text: 'Try it again', onclick: () => again(row) }), ' ');
  }
  li.append(el('button', {
    class: 'lnk plain', type: 'button', text: detail ? 'Take theirs' : 'Let it go',
    onclick: () => resolve(row, null),
  }));
  return li;
}

// whole is the text itself rather than the beginning of it, in a textarea so
// that it scrolls, wraps and can be selected and copied out. Read only: this is
// the record of something that was not saved, and editing it here would write
// nowhere and read as though it had.
function whole(text) {
  const area = el('textarea', { class: 'mono kept', readonly: true, spellcheck: 'false',
    'aria-label': 'What you wrote, which was not saved' });
  area.value = text;
  return el('div', {}, el('p', { class: 'mono dim', text: 'Yours, in full:' }), area);
}

// resolve is what the choice comes to: send it again as it now has to be sent,
// or drop it and put the row back to what the server says it holds. Either way
// the entry leaves the outbox, because the choice has been made and offering it
// a second time would be asking twice. The row carries the proposition it was
// made on, which need not be the open one.
async function resolve(row, args) {
  if (!args) {
    await letGo(row);
    return;
  }
  try {
    await resend(row, args);
  } catch (err) {
    say(err.message);
  }
}

function when(unix) {
  const seconds = Math.max(0, Math.floor(Date.now() / 1000) - unix);
  if (seconds < 60) return 'just now';
  if (seconds < 3600) return Math.floor(seconds / 60) + ' min ago';
  if (seconds < 86400) return Math.floor(seconds / 3600) + ' h ago';
  return new Date(unix * 1000).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

// outstanding is what the tab says beside its name: the queue plus whatever the
// server has already refused.
export const outstanding = () => state.waitingHere + state.refused.length;

async function openTrash() {
 const prop=state.open;
 const {dialog,body}=modal('Recently deleted');
 let saving=false;
 const dismiss=()=>{if(!saving)dialog.close();};dialog.querySelector('header button').onclick=dismiss;dialog.addEventListener('cancel',e=>{e.preventDefault();dismiss();});
 const draw=async()=>{
  clear(body).append(el('p',{role:'status',text:'Loading deleted items…'}));
  try{
   const {items}=await api.get('/trash?proposition='+prop);if(!dialog.open)return;
   clear(body).append(el('p',{text:'Individual cards, documents, links, files, evidence, and calendar events can be restored for seven days. Permanent proposition deletion and incomplete uploads are excluded.'}));
   if(!items.length)body.append(el('p',{text:'No recoverable deleted items.'}));
   for(const item of items){
    const attempt=api.mutation();
    const status=el('p',{role:'status'});
    const restore=el('button',{type:'button',class:'act',text:'Restore',onclick:async()=>{
     if(saving)return;saving=true;restore.disabled=true;status.textContent='Restoring…';
     try{const answer=await attempt.run('POST','/trash/'+item.id+'/restore',{});apply(answer.event);await draw();say('Item restored');}
     catch(err){status.textContent=err.message;restore.disabled=false;}
     finally{saving=false;}
    }});
    body.append(el('article',{class:'review-item'},el('strong',{text:item.title||item.entity+' #'+item.entity_id}),el('p',{class:'dim',text:item.entity.replaceAll('_',' ')+' · recover until '+new Date(item.expires_at*1000).toLocaleString()}),state.can.delete?restore:null,status));
   }
  }catch(err){clear(body).append(el('p',{role:'status',text:err.message}),el('button',{class:'lnk',type:'button',text:'Retry',onclick:draw}));}
 };await draw();
}
