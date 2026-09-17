// Confirmations are real dialogs: a button names one by id, and any button
// inside it that is not a submit closes it. No inline handlers, no confirm().
document.addEventListener('click', function (e) {
  var opener = e.target.closest('[data-dialog]');
  if (opener) {
    var d = document.getElementById(opener.dataset.dialog);
    if (d) {
      e.preventDefault();
      d.showModal();
    }
    return;
  }
  var closer = e.target.closest('[data-close]');
  if (closer) {
    e.preventDefault();
    closer.closest('dialog').close();
  }
});
