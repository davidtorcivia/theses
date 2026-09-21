import { calendarDay } from './filters.js';
import { el, clear, inline } from './dom.js';
import { state, byHandle, proposition } from './state.js';
import * as api from './api.js';

const sections = new Map();
export function connectedSection(title, path) {
  const key = state.me + ':' + state.open + ':' + path;
  const scope = JSON.stringify(state.props);
  const cached = sections.get(key);
  if (cached) {
    if (cached.scope !== scope) { cached.scope = scope; cached.refresh(); }
    return cached.details;
  }
  const body = el('div');
  const details = el('details', { class: 'connected' }, el('summary', { text: title }), body);
  let request = 0;
  const refresh = async () => {
    const mine = ++request;
    if (body.contains(document.activeElement)) details.querySelector('summary').focus();
    clear(body);
    if (!details.open) return;
    body.append(el('p', { role: 'status', text: 'Loading…' }));
    try {
      const answer = await api.get(path);
      if (mine !== request || !details.isConnected) return;
      clear(body);
      const items = (answer.items || []).filter((item) => proposition(item.proposition_id));
      body.append(el('button', { class: 'lnk', type: 'button', text: 'Refresh', onclick: refresh }));
      if (!items.length) body.append(el('p', { class: 'dim', text: 'Nothing here yet.' }));
      const today = calendarDay(state.timezone);
      let group = '';
      for (const item of items) {
        if (path === '/my-work') {
          const next = !item.due_date ? 'No deadline' : item.due_date < today ? 'Overdue' : item.due_date === today ? 'Due today' : 'Upcoming';
          if (group !== next) { group = next; body.append(el('h3', { text: group })); }
        }
        const p = proposition(item.proposition_id);
        const label = el('span', {}, inline(item.title, byHandle, proposition), ' ', el('a', { href: item.url, text: 'Open ' + item.kind, onclick: (e) => {
          if (item.proposition_id === state.open && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey && e.button === 0) {
            e.preventDefault(); location.hash = new URL(item.url, location.origin).hash;
          }
        } }));
        body.append(el('article', {}, label,
          el('span', { class: 'dim', text: ' · ' + (p?.title || 'Workspace') + (item.due_date ? ' · due ' + item.due_date : '') }),
          item.text ? el('p', {}, inline(item.text, byHandle, proposition)) : null));
      }
      if (path === '/my-work') {
        const unresolved = state.refused.length;
        if (unresolved) body.append(el('a', { href: '#activity', text: `${unresolved} local items need attention` }));
      }
    } catch {
      if (mine !== request) return;
      clear(body).append(el('p', { role: 'status', text: 'Could not load. Close and reopen to retry.' }));
    }
  };
  details.addEventListener('toggle', refresh);
  sections.set(key, { details, refresh, scope });
  return details;
}
