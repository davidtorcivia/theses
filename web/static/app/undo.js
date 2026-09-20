// What Ctrl+Z takes back inside one block, kept by this tab. Nothing here
// touches the DOM or the state, so it runs under node as well as in the
// browser, which is what web/undo_test.mjs does.
//
// A textarea keeps an undo stack of its own and throws it away the moment a
// script writes to its value. The editor writes to it all the time: a merge the
// server made folded in, Enter continuing a list, a paste of several
// paragraphs, the head a split keeps, text put back after a refusal. So after
// any of those the browser's Ctrl+Z does nothing, or something wrong, and the
// editor has to keep the history itself.
//
// A snapshot is the whole text and where the person was standing in it, not an
// edit: a block is a paragraph, two hundred of them is what one block holds two
// hundred times over, and an edit list would have to be carried through every
// one of those writes rather than simply thrown away when one arrives.

// together is how long a pause in the typing ends a run, so that a run of
// keystrokes is one step rather than one step each.
export const together = 800;

// cap is how many steps a block keeps. The oldest goes when a new one arrives
// over it.
export const cap = 200;

// history is one block's steps and where in them the block now is. kind tells a
// run of typing from a run of deleting and from a step that stands alone, so
// that backspacing over a word and then writing another are two things to take
// back rather than one.
export function history(text, start = text.length, end = start) {
  const list = [{ text, start, end }];
  let at = 0;
  let kindWas = '';
  let whenWas = 0;

  // moved is undo and redo, which are the same walk in two directions. A run
  // ends here whichever way it goes: somebody who has asked for their typing
  // back is not still typing it.
  const moved = (by) => {
    const to = at + by;
    if (to < 0 || to >= list.length) return null;
    at = to;
    kindWas = '';
    return { ...list[at] };
  };

  return {
    // record is one change to the text. kind is 'type' for text going in, 'cut'
    // for text coming out and 'step' for anything that stands on its own: a
    // write the script made, a composed word an input method finished, a paste
    // or a drop the editor did not take over. from is where the selection was
    // before this change, which is how a run ends when somebody clicks
    // somewhere else in the block and carries on writing there.
    record(snap, kind = 'step', when = Date.now(), from = null) {
      const top = list[at];
      // The same text at another caret is not a change. It moves where an undo
      // would put the person back and nothing else: not the clock this run is
      // measured by, and not the redo that may be sitting ahead of here.
      if (top.text === snap.text) {
        top.start = snap.start;
        top.end = snap.end;
        return;
      }
      const join = kind !== 'step' && kind === kindWas && when - whenWas <= together
        && Boolean(from) && from.start === top.start && from.end === top.end;
      kindWas = kind;
      whenWas = when;
      if (join) {
        list[at] = snap;
        return;
      }
      // Anything recorded after an undo is a new branch of what happened, and
      // the redo it would have gone back to is no longer a thing that happened.
      list.length = at + 1;
      list.push(snap);
      at++;
      if (list.length > cap) {
        list.shift();
        at--;
      }
    },
    // undo and redo answer the snapshot they moved to, or nothing when there
    // is nowhere left to go, which is the whole of what the editor asks about
    // whether either is possible.
    undo: () => moved(-1),
    redo: () => moved(1),
    // now is the text this history believes the block holds. A block that
    // reads something else when it is opened again moved while nobody here was
    // looking at it, and every step in here was taken against text that is no
    // longer what anybody would be undoing out of.
    now: () => list[at].text,
    // steps is what the list holds, for the test to count.
    steps: () => list.length,
  };
}

// kept is every block this tab has a history for, by block id. A history
// outlives the editor being given up: leaving a paragraph and coming back to it
// is not giving up what you wrote in it.
const kept = new Map();

// of is a block's history, started from the text it holds now when there is
// none yet.
export function of(id, snap) {
  let h = kept.get(id);
  if (!h) {
    h = history(snap.text, snap.start, snap.end);
    kept.set(id, h);
  }
  return h;
}

// reset throws a block's history away and begins again from the text it holds
// now. It is what somebody else's words arriving in the middle of this block
// come to: every snapshot above was taken against text those words are not in,
// so undoing to one would take them back out and save that.
export function reset(id, text, start = text.length, end = start) {
  kept.set(id, history(text, start, end));
}

// move carries a history from one block id to another. A block made in this tab
// is drawn under an id of its own until the server answers, and what was typed
// into it while it waited is the same paragraph's to take back once it has the
// id the server gave it. A block with nothing to take back has no history and
// nothing to carry.
export function move(from, to) {
  const h = kept.get(from);
  if (!h) return;
  kept.delete(from);
  kept.set(to, h);
}

// keep drops the histories of blocks that are not on the page any more: one
// somebody deleted, and every block of a document that is no longer the open
// one.
export function keep(ids) {
  for (const id of kept.keys()) if (!ids.has(id)) kept.delete(id);
}
