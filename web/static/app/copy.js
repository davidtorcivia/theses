// The Copy button beside a value the settings page shows once. The value is
// selectable in one click without this, so all the button saves is the
// selection; a browser that refuses the clipboard says so rather than pretend.

for (const button of document.querySelectorAll('[data-copy]')) {
  const value = document.getElementById(button.dataset.copy);
  if (!value) continue;
  button.addEventListener('click', async () => {
    const was = button.textContent;
    try {
      await navigator.clipboard.writeText(value.textContent.trim());
      button.textContent = 'Copied';
    } catch {
      button.textContent = 'Select it and copy it';
    }
    setTimeout(() => { button.textContent = was; }, 2000);
  });
}
