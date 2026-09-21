import { newKey } from './net.js';
import { el, clear, say, ask } from './dom.js';
import { state, user, canEdit, apply } from './state.js';
import * as api from './api.js';

export function editFileNotes(file) {
  const note = el('textarea', { rows: 8, value: file.note_md || '', 'aria-label': 'File notes' });
  note.value = file.note_md || '';
  const tags = el('input', { value: file.tags || '', 'aria-label': 'Tags', maxlength: 400 });
  const status = el('p', { role: 'status' });
  const choices = el('div', { class: 'acts' });
  const save = el('button', { type: 'submit', class: 'save', text: 'Save notes' });
  const cancel = el('button', { type: 'button', class: 'lnk', text: 'Cancel' });
  const form = el('form', {}, el('h3', { id: 'file-notes-title', text: 'File notes and tags' }),
    el('p', { text: 'Saved online. Separate up to 12 tags with commas.' }), note, tags, status, choices, el('div', { class: 'acts' }, cancel, save));
  const dialog = el('dialog', { 'aria-labelledby': 'file-notes-title' }, form);
  let version = file.metadata_version;
  let saved = false;
  const dirty = () => !saved && (note.value !== (file.note_md || '') || tags.value !== (file.tags || ''));
  const close = async () => { if (save.disabled) return; if (!dirty() || await ask('Discard these file notes?', 'Your changes have not been saved.', 'Discard')) dialog.close(); };
  cancel.onclick = close;
  dialog.addEventListener('cancel', (e) => { e.preventDefault(); close(); });
  const leaving = (e) => { if (dirty()) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('beforeunload', leaving);
  const back = document.activeElement;
  dialog.addEventListener('close', () => { window.removeEventListener('beforeunload', leaving); dialog.remove(); if (back?.isConnected) back.focus(); });
  form.onsubmit = async (e) => {
    e.preventDefault(); save.disabled = note.disabled = tags.disabled = cancel.disabled = true; clear(choices); status.textContent = 'Saving…';
    try {
      const answer = await api.patch('/files/' + file.id, { note_md: note.value, tags: tags.value, metadata_version: version });
      saved = true; apply(answer.event); dialog.close();
    } catch (err) {
      status.textContent = err.status === 409 ? 'Someone changed these notes. Your text is still here. Choose which version to keep.' : err.message;
      if (err.status === 409) {
        try {
          const { file: latest } = await api.get('/files/' + file.id);
          choices.append(el('pre', { text: latest.note_md + '\nTags: ' + latest.tags }),
            el('button', { type: 'button', class: 'lnk', text: 'Use latest', onclick: () => { note.value = latest.note_md; tags.value = latest.tags; version = latest.metadata_version; clear(choices); status.textContent = 'Latest version loaded.'; } }),
            el('button', { type: 'button', class: 'lnk', text: 'Keep my text', onclick: () => { version = latest.metadata_version; clear(choices); status.textContent = 'Press Save notes to replace the displayed version with yours.'; } }));
        } catch { status.textContent += ' Could not load the latest version; retry when connected.'; }
      }
    } finally { save.disabled = note.disabled = tags.disabled = cancel.disabled = false; }
  };
  document.body.append(dialog); dialog.showModal();
}

const recordings = new Map();
export function recordingComments(file) {
  let entry = recordings.get(file.id);
  if (!entry) {
    entry = { node: el('section', {}), revision: -1, request: 0 };
    recordings.set(file.id, entry);
  }
  if (entry.revision !== file.comment_revision) {
    entry.revision = file.comment_revision;
    loadComments(file, entry);
  }
  return entry.node;
}

async function loadComments(file, entry) {
  const request = ++entry.request;
  entry.restoreFocus ||= entry.node.contains(document.activeElement);
  clear(entry.node).append(el('h4', { text: 'Recording comments' }));
  try {
    const answer = await api.get('/files/' + file.id + '/comments');
    if (request !== entry.request) return;
    for (const comment of answer.comments || []) {
      const seconds = Math.floor(comment.position_ms / 1000);
      const stamp = `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
      entry.node.append(el('p', {}, el('button', { type: 'button', class: 'lnk', text: stamp, onclick: () => {
        const player = document.querySelector('#drawer audio, #drawer video');
        if (player) player.currentTime = comment.position_ms / 1000;
        else say('Download the recording to view this timestamp.');
      } }), ' · ' + user(comment.user_id).name + ' · ' + comment.body_md + (comment.resolved_at?' · Resolved':''),
 canEdit()?el('button',{type:'button',class:'lnk',text:comment.resolved_at?'Reopen':'Resolve',onclick:async(e)=>{e.currentTarget.disabled=true;try{const answer=await api.patch(`/files/${file.id}/comments/${comment.id}`,{resolved:!comment.resolved_at,version:comment.version});apply(answer.event);await loadComments(answer.file,entry);}catch(err){say(err.message);loadComments(file,entry);}}}):null,
      canEdit() && comment.user_id === state.me ? el('button', { type: 'button', class: 'lnk', text: 'Delete', onclick: async () => {
        if (!await ask('Delete this recording comment?', '', 'Delete')) return;
        try { const answer = await api.del(`/files/${file.id}/comments/${comment.id}`); entry.revision = answer.file.comment_revision; entry.restoreFocus = true; apply(answer.event); await loadComments(answer.file, entry); } catch (err) { say(err.message); }
      } }) : null));
    }
    if (!answer.comments?.length) entry.node.append(el('p', { class: 'dim', text: 'No recording comments yet.' }));
    if (canEdit()) entry.node.append(el('button', { type: 'button', class: 'lnk', 'data-add-comment': '', text: 'Add timestamped comment', onclick: () => addComment(file, entry) }));
  } catch (err) {
    if (request !== entry.request) return;
    entry.node.append(el('p', { text: err.message }), el('button', { type: 'button', class: 'lnk', text: 'Retry comments', onclick: () => loadComments(file, entry) }));
  }
  if (entry.restoreFocus) { entry.restoreFocus = false; (entry.node.querySelector('[data-add-comment]') || entry.node.querySelector('button'))?.focus(); }
}

function addComment(file, entry) {
  const player = document.querySelector('#drawer audio, #drawer video');
  const seconds = el('input', { type: 'number', min: 0, max: file.duration_ms ? file.duration_ms / 1000 : 86400, step: 1, value: Math.floor(player?.currentTime || 0), 'aria-label': 'Timestamp in seconds', required: true });
  const body = el('textarea', { rows: 4, 'aria-label': 'Recording comment', required: true });
  const status = el('p', { role: 'status' });
  const save = el('button', { type: 'submit', class: 'save', text: 'Add comment' });
  const close = el('button', { type: 'button', class: 'lnk', text: 'Cancel' });
  const form = el('form', {}, el('h3', { id: 'recording-comment-title', text: 'Recording comment' }), el('p', { text: 'Timestamp in seconds. Saved online.' }), seconds, body, status, el('div', { class: 'acts' }, close, save));
  const dialog = el('dialog', { 'aria-labelledby': 'recording-comment-title' }, form);
  let attempt = null;
  const dismiss = async () => {
    if (save.disabled) return;
    if (!body.value || await ask('Discard this comment?', attempt ? 'The last request may have succeeded. Retry to confirm it without duplicating it.' : 'Your comment has not been saved.', 'Discard')) dialog.close();
  };
  close.onclick = dismiss;
  dialog.addEventListener('cancel', (e) => { e.preventDefault(); dismiss(); });
  const back = document.activeElement;
  const leaving = (e) => { if (body.value) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('beforeunload', leaving);
  dialog.addEventListener('close', () => { window.removeEventListener('beforeunload', leaving); dialog.remove(); if (back?.isConnected) back.focus(); else entry.node.querySelector('[data-add-comment]')?.focus(); });
  form.onsubmit = async (e) => {
    e.preventDefault(); save.disabled = true;
    attempt ||= { key: newKey(), body: { position_ms: Math.round(Number(seconds.value) * 1000), body_md: body.value } };
    seconds.disabled = body.disabled = true;
    try {
      const answer = await api.post('/files/' + file.id + '/comments', attempt.body, { 'Idempotency-Key': attempt.key });
      entry.revision = answer.file.comment_revision; apply(answer.event); await loadComments(answer.file, entry); dialog.close();
    } catch (err) {
      status.textContent = err.message;
      if (err.status && err.status < 500) { attempt = null; seconds.disabled = body.disabled = false; }
      else status.textContent += ' Retry sends the same comment once.';
    } finally { save.disabled = false; }
  };
  document.body.append(dialog); dialog.showModal();
}
