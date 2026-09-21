const people = document.querySelector('#people');
const add = document.querySelector('#add-person');
let nextPerson = people?.children.length || 0;
function renumber() {
  [...people.children].forEach((person, i) => { person.querySelector('[data-person-number]').textContent = i + 1; });
  add.disabled = people.children.length >= 20;
}
add?.addEventListener('click', () => {
  if (people.children.length >= 20) return;
  const person = people.firstElementChild.cloneNode(true);
  const index = nextPerson++;
  for (const input of person.querySelectorAll('input')) {
    input.name = input.name.replace(/^person_\d+_/, `person_${index}_`);
    if (input.name === 'person') input.value = index;
    else if (input.type === 'checkbox') input.checked = false;
    else if (input.type !== 'date') input.value = '';
  }
  person.querySelector('input[type="email"]').required = false;
  people.append(person);
  renumber();
  person.querySelector('input[autocomplete="name"]').focus();
});
people?.addEventListener('click', (event) => {
  const remove = event.target.closest('.remove-person');
  if (remove && people.children.length > 1) { remove.closest('.person').remove(); renumber(); add.focus(); }
});
people?.addEventListener('change', (event) => {
  const person = event.target.closest('.person');
  if (person) person.querySelector('input[type="email"]').required = person.querySelector('input[name$="_notify"]').checked;
});
const signForm = document.querySelector('#sign-form');
signForm?.addEventListener('submit', () => {
  signForm.querySelector('button[type="submit"]').disabled = true;
  document.querySelector('#sign-status').textContent = 'Saving your signed release…';
});
window.addEventListener('pageshow', () => {
  if (signForm) { signForm.querySelector('button[type="submit"]').disabled = false; document.querySelector('#sign-status').textContent = ''; }
});
const kindInput = document.querySelector('#release-kind');
let previousKind = kindInput?.value;
kindInput?.addEventListener('change', () => {
  const body = document.querySelector('#release-body');
  const oldDraft = document.querySelector('#draft-' + previousKind)?.content.textContent || '';
  if (body.value === oldDraft || body.value === '') body.value = document.querySelector('#draft-' + kindInput.value)?.content.textContent || '';
  previousKind = kindInput.value;
});
document.querySelector('#use-release-template')?.addEventListener('click', () => {
  const kind = document.querySelector('#release-kind').value;
  const draft = document.querySelector('#draft-' + kind);
  if (draft) { document.querySelector('#release-body').value = draft.content.textContent; }
});
document.querySelector('#copy-link')?.addEventListener('click', async () => {
  const input = document.querySelector('#release-url');
  try { await navigator.clipboard.writeText(input.value); document.querySelector('#copy-status').textContent = 'Link copied.'; }
  catch { input.focus(); input.select(); document.querySelector('#copy-status').textContent = 'Select and copy the link above.'; }
});
for (const button of document.querySelectorAll('[data-print]')) button.addEventListener('click', () => window.print());
document.querySelector('#email-preview')?.focus();
