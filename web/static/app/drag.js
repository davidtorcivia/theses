// One drag for the whole app. A mobile browser fires no HTML5 drag event from
// a finger, so on a phone that mechanism cannot reach the board or the rail at
// all. Pointer events are the single path that carries a mouse, a pen and a
// finger, and this is the whole of it.
//
// Only the moment a drag begins differs between them. A mouse with the button
// down that has moved a few pixels is dragging, because a mouse never scrolls
// the page this way. A finger is doing one of three things, and they are told
// apart in time: one that stays put long enough for the press to be meant is
// carrying the row, one that moves before then is scrolling the page, and one
// that lifts before then has tapped to open what is under it. A finger on a
// handle asks none of that, because a handle is for nothing else, so that one
// begins the way a mouse does.

import { hold } from './state.js';
import { say } from './dom.js';

const PRESS = 300;
const SLOP = 6;

// How near an edge brings the page up to meet the finger, and how fast.
const EDGE = 64;
const SPEED = 12;

// One row at a time, whatever is holding it. A second finger on a second row
// would send a second move and rebuild the list under the first one.
let active = null;

// The pointer that has just carried a row ends in a click as well, and that
// one finishes the drag rather than asking to open what it landed on. It is
// the one click it swallows, and the next press clears it anyway, because
// where the click lands after a row has been reparented is the browser's
// business and it may not land on the row at all. A press whose row was rebuilt
// under it swallows its click too, because the finger was holding a row rather
// than asking to open one.
let carried = false;

export function carrying() {
  const was = carried;
  carried = false;
  return was;
}

// Any next press clears it, whether or not what is pressed can be carried. A
// row that asks the question without being movable, which an archived one is,
// would otherwise answer for a drag that ended somewhere else entirely. In the
// capture phase, so the press that picks a row up clears it before it runs.
addEventListener('pointerdown', () => { carried = false; }, true);

// Whether the row in the air is being carried by a finger, which is the only
// case where a touchmove has to be refused.
let touchDrag = false;

// Neither value of the touch action property answers this. The browser reads
// the rule when the finger lands, a third of a second before the press that
// picks the row up, so a rule written at either moment is read too late to stop
// the scroll, and a card that refused touch action outright would stop the
// board scrolling under every finger that lands on one, which on a phone is
// most of the screen. Refusing touchmove does stop it: the press needed the
// finger still, so no scroll has begun. The refusal is registered here, once,
// at import, because Safari does not honor a preventDefault from a touchmove
// listener that was added after the touch began.
addEventListener('touchmove', (ev) => {
  if (touchDrag && ev.cancelable) ev.preventDefault();
}, { passive: false });

// A phone has no console, so with dragdebug set in local storage the bar names
// what ended each drag. This goes away once a device has shown whether the
// press holds.
const debug = () => {
  try { return localStorage.getItem('dragdebug'); } catch { return null; }
};

// movable carries node with the pointer. zone is the selector of a container a
// row may be let go over, list finds the element inside one whose children are
// the rows, rows is the selector of those among that element's children, over
// is a class marking the zone under the pointer, and drop is handed the zone
// the row was let go over. A release anywhere else moves nothing, the same as
// Escape.
//
// handle is a selector inside the row, and with one given a press anywhere else
// on the row is left alone: a document block is dragged by a grip in its margin
// because pressing the text of one has to go on meaning what it means, which is
// click to write and drag to select.
export function movable(node, { zone: zoneSel, list = (z) => z, rows: rowSel = '',
  over = '', handle = '', drop }) {
  // Dragging a row is not selecting the text on it, and the row left behind is
  // never what the pointer is over. A row with a handle keeps both: the rule
  // belongs on the handle, and the text of a block stays selectable.
  if (!handle) node.classList.add('movable');

  node.addEventListener('pointerdown', (e) => {
    // A button on the row answers for itself, a second mouse button is not a
    // drag, and a row is already in somebody's hand.
    if (active || e.button !== 0 || e.target.closest('button')) return;
    if (handle) {
      if (!e.target.closest(handle)) return;
      // A press on a handle is never a request to open what is under it,
      // however little it moves afterwards, so the click it ends in is
      // swallowed from here rather than from the end of a drag that may never
      // begin. It has to be swallowed by the row rather than by the handle:
      // the line below takes the compatibility mouse events away, and the
      // click the browser makes without them is aimed at the row.
      carried = true;
      // The press is answered here and nowhere else. Letting it through would
      // move the focus to the row, and on a block that is the open editor
      // somewhere else on the page blurring and closing before the drag has
      // even begun.
      e.preventDefault();
    }

    const token = {};
    const mouse = e.pointerType === 'mouse';
    // Only a finger that might be scrolling the page instead has to hold still
    // first. A finger that landed on a handle is not one of those: the handle
    // is for nothing else and refuses touch action, so nothing is scrolling
    // under it.
    const holds = !handle && !mouse;
    const began = Date.now();
    const from = { x: e.clientX, y: e.clientY };
    let at = { ...from };
    let grab = null;
    let ghost = null;
    let zone = null;
    let on = false;
    let edge = 0;
    let frame = 0;
    let timer = holds ? setTimeout(start, PRESS) : 0;

    // Android answers a long press with a context menu and cancels the pointer
    // behind it, which would drop the row in the moment it was picked up.
    const quiet = (ev) => ev.preventDefault();

    active = token;
    // The platform's own long press lands at around half a second, after the
    // press that picks the row up, and whatever it starts takes the touch away
    // for good. Selection is refused for the whole document from the moment the
    // finger lands, well before that, because iOS selects the nearest
    // selectable text even when what was pressed is not selectable itself.
    if (!mouse) document.documentElement.classList.add('pressing');
    node.setPointerCapture(e.pointerId);
    addEventListener('pointermove', moved);
    addEventListener('pointerup', up);
    addEventListener('pointercancel', end);
    addEventListener('contextmenu', quiet);
    // A pointer that never comes back, because another window took the focus
    // or the finger was released over another app, would otherwise leave the
    // row in the air and rendering held for good.
    addEventListener('blur', end);
    // Ahead of the listener on the document, which would read the same key as
    // a request to close whatever else is open.
    addEventListener('keydown', abandon, true);

    function start() {
      timer = 0;
      // Any change applied during the press rebuilt the list and took this row
      // out of the page, because rendering is only held from here on. The node
      // in hand is a stale one; putting it back among the fresh rows would show
      // the card twice until the drop drew the board again.
      if (!node.isConnected) { carried = true; end(); return; }
      on = true;
      touchDrag = !mouse;
      const box = node.getBoundingClientRect();
      grab = { x: at.x - box.left, y: at.y - box.top };
      ghost = node.cloneNode(true);
      ghost.classList.add('ghost');
      ghost.removeAttribute('tabindex');
      ghost.style.width = box.width + 'px';
      document.body.append(ghost);
      node.classList.add('dragging');
      // A change arriving mid drag would rebuild the list and take the row out
      // of the hand holding it, so rendering waits until the drop.
      hold(true);
      frame = requestAnimationFrame(tick);
      follow();
      place();
    }

    function follow() {
      ghost.style.left = (at.x - grab.x) + 'px';
      ghost.style.top = (at.y - grab.y) + 'px';
    }

    // place puts the row where the pointer is, by the rule the mockup drew:
    // among the rows of the list under the pointer, above the first one whose
    // middle the pointer has not reached. Off every list it leaves the row
    // where it lies and remembers that there is nowhere to drop it.
    function place() {
      const found = document.elementFromPoint(at.x, at.y)?.closest(zoneSel) || null;
      if (found !== zone) {
        if (zone && over) zone.classList.remove(over);
        zone = found;
      }
      if (!zone) return;
      if (over) zone.classList.add(over);
      const rows = list(zone);
      const here = [...rows.children].filter((c) => c !== node && (!rowSel || c.matches(rowSel)));
      const above = here.find((c) => at.y < c.getBoundingClientRect().top + c.offsetHeight / 2);
      // A list with no other row in it is either one this row is already the
      // whole of, where there is nothing to place it against, or an empty
      // column it is joining.
      if (!above && !here.length && node.parentNode === rows) return;
      // Past the last row the row goes directly after it rather than at the end
      // of the list, because what follows the rows is not a row: under a
      // document's blocks stands the button that adds one, and nothing may be
      // dropped below that.
      const before = above || (here.length ? here.at(-1).nextSibling : null);
      // A row already where it belongs is left where it is. Putting it back
      // takes it out of the page for the instant it takes to insert it again,
      // and a node taken out releases the pointer capture the finger holding it
      // has. Where it belongs is in this list: a row arriving from another one
      // is always inserted, however its old neighbors happen to line up.
      if (before !== node && !(node.parentNode === rows && node.nextSibling === before)) {
        rows.insertBefore(node, before);
      }
    }

    // A finger at the edge of a phone cannot reach the column below the fold,
    // so the page comes to it. The pointer holds still while this runs and the
    // board moves under it, so the row is placed again on every frame.
    //
    // ponytail: the window is what moves. A list in a panel that scrolls on its
    // own, which the rail is on a phone, stays where it is; give the panel the
    // scroll when somebody has more propositions than a screen holds.
    function tick() {
      if (edge) { scrollBy(0, edge); place(); }
      frame = requestAnimationFrame(tick);
    }

    function moved(ev) {
      if (ev.pointerId !== e.pointerId) return;
      at = { x: ev.clientX, y: ev.clientY };
      if (!on) {
        if (Math.abs(at.x - from.x) <= SLOP && Math.abs(at.y - from.y) <= SLOP) return;
        if (holds) end(); else start();
        return;
      }
      follow();
      place();
      edge = mouse ? 0 : at.y < EDGE ? -SPEED : at.y > innerHeight - EDGE ? SPEED : 0;
    }

    function up(ev) {
      if (ev.pointerId !== e.pointerId) return;
      // A drop that throws still puts the row down, or the document would stay
      // unselectable and the page unable to scroll.
      try { if (on && zone) drop(zone); } finally { end(ev); }
    }

    // Escape puts the row down. Nothing is sent, and the render held through
    // the drag draws the list back out of the state, which is where the row
    // never stopped being.
    function abandon(ev) {
      if (ev.key !== 'Escape' || !on) return;
      ev.preventDefault();
      ev.stopPropagation();
      end(ev);
    }

    // ev is the event that ended the drag, which pointercancel and blur pass
    // themselves and the rest hand over, because which one it was is the only
    // thing a phone can be asked about a drag that let go by itself.
    function end(ev) {
      if (active === token) active = null;
      touchDrag = false;
      document.documentElement.classList.remove('pressing');
      clearTimeout(timer);
      cancelAnimationFrame(frame);
      removeEventListener('pointermove', moved);
      removeEventListener('pointerup', up);
      removeEventListener('pointercancel', end);
      removeEventListener('contextmenu', quiet);
      removeEventListener('blur', end);
      removeEventListener('keydown', abandon, true);
      if (node.hasPointerCapture(e.pointerId)) node.releasePointerCapture(e.pointerId);
      if (!on) return;
      on = false;
      carried = true;
      ghost.remove();
      node.classList.remove('dragging');
      if (zone && over) zone.classList.remove(over);
      zone = null;
      hold(false);
      if (debug()) say(`drag ended by ${ev.key || ev.type} after ${Date.now() - began}ms`);
    }
  });
}
