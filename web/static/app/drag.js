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
// that lifts before then has tapped to open what is under it.

import { hold } from './state.js';

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
// business and it may not land on the row at all.
let carried = false;

export function carrying() {
  const was = carried;
  carried = false;
  return was;
}

// movable carries node with the pointer. zone is the selector of a container a
// row may be let go over, list finds the element inside one whose children are
// the rows, over is a class marking the zone under the pointer, and drop is
// handed the zone the row was let go over. A release anywhere else moves
// nothing, the same as Escape.
export function movable(node, { zone: zoneSel, list = (z) => z, over = '', drop }) {
  // Dragging a row is not selecting the text on it, and the row left behind is
  // never what the pointer is over.
  node.classList.add('movable');

  node.addEventListener('pointerdown', (e) => {
    carried = false;
    // A button on the row answers for itself, a second mouse button is not a
    // drag, and a row is already in somebody's hand.
    if (active || e.button !== 0 || e.target.closest('button')) return;

    const token = {};
    const mouse = e.pointerType === 'mouse';
    const from = { x: e.clientX, y: e.clientY };
    let at = { ...from };
    let grab = null;
    let ghost = null;
    let zone = null;
    let on = false;
    let edge = 0;
    let frame = 0;
    let press = mouse ? 0 : setTimeout(start, PRESS);

    // Neither value of the touch action property answers this. The browser
    // reads the rule when the finger lands, a third of a second before the
    // press that picks the row up, so a rule written at either moment is read
    // too late to stop the scroll. Refusing the first touchmove of the drag
    // does stop it: the press needed the finger still, so no scroll has begun.
    const still = (ev) => ev.preventDefault();
    // Android answers a long press with a context menu and cancels the pointer
    // behind it, which would drop the row in the moment it was picked up.
    const quiet = (ev) => ev.preventDefault();

    active = token;
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
      press = 0;
      on = true;
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
      addEventListener('touchmove', still, { passive: false });
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
      const after = [...rows.children]
        .find((c) => c !== node && at.y < c.getBoundingClientRect().top + c.offsetHeight / 2);
      after ? rows.insertBefore(node, after) : rows.append(node);
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
        if (mouse) start(); else end();
        return;
      }
      follow();
      place();
      edge = mouse ? 0 : at.y < EDGE ? -SPEED : at.y > innerHeight - EDGE ? SPEED : 0;
    }

    function up(ev) {
      if (ev.pointerId !== e.pointerId) return;
      if (on && zone) drop(zone);
      end();
    }

    // Escape puts the row down. Nothing is sent, and the render held through
    // the drag draws the list back out of the state, which is where the row
    // never stopped being.
    function abandon(ev) {
      if (ev.key !== 'Escape' || !on) return;
      ev.preventDefault();
      ev.stopPropagation();
      end();
    }

    function end() {
      if (active === token) active = null;
      clearTimeout(press);
      cancelAnimationFrame(frame);
      removeEventListener('pointermove', moved);
      removeEventListener('pointerup', up);
      removeEventListener('pointercancel', end);
      removeEventListener('contextmenu', quiet);
      removeEventListener('blur', end);
      removeEventListener('keydown', abandon, true);
      removeEventListener('touchmove', still);
      if (node.hasPointerCapture(e.pointerId)) node.releasePointerCapture(e.pointerId);
      if (!on) return;
      on = false;
      carried = true;
      ghost.remove();
      node.classList.remove('dragging');
      if (zone && over) zone.classList.remove(over);
      zone = null;
      hold(false);
    }
  });
}
