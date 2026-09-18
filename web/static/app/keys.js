// Keyboard reach for the things that are not buttons. A card is an article and
// a rail row is a list item, because both drag and both hold buttons of their
// own, and a button may not contain a button. A heading that opens an editor on
// a click is a heading and stays one, because its level is worth more to
// somebody listening than the word button is. All of them still have to be
// reachable with a keyboard, so they take the focus and answer the two keys.

export function activate(node, run) {
  node.tabIndex = 0;
  node.addEventListener('keydown', (e) => {
    // Enter on a button inside the row belongs to that button, and a heading
    // already being edited wants both keys for itself.
    if (e.target !== node || node.isContentEditable) return;
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault();
    run(e);
  });
}
