import { calendarDay } from './filters.js';
import { el, clear, inline } from './dom.js';
import { state, byHandle, proposition } from './state.js';
import * as api from './api.js';

const sections = new Map();
export function connectedSection(title, path) {
  const key = state.me + ':' + state.open + ':' + path;
  const scope = JSON.stringify(state.props)+(path==='/my-work'?':'+state.seq:'');
  const cached = sections.get(key);
  if (cached) {
    if (cached.scope !== scope) { cached.scope = scope; cached.refresh(); }
    return cached.details;
  }
  const body = el('div');
  const mineWork=path==='/my-work';
  const details = el('details', { class: 'connected'+(mineWork?' my-work':'') }, el('summary', {},mineWork?el('span',{class:'section-eyebrow',text:'YOUR FOCUS'}):null,el('span',{text:title})), body);
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
      if (!items.length) body.append(el('div', { class:'work-empty' },el('strong',{text:mineWork?'A clear desk.':'No references yet.'}),el('p',{class:'dim',text:mineWork?'Tasks assigned to you will appear here, organized by deadline.':'Linked passages will appear here.'})));
      if(mineWork&&items.length)body.prepend(el('p',{class:'work-count',text:items.length+' open tasks assigned to you'}));
      const today = calendarDay(state.timezone);
      let group = '';
      for (const item of items) {
        if (path === '/my-work') {
          const next = !item.due_date ? 'No deadline' : item.due_date < today ? 'Overdue' : item.due_date === today ? 'Due today' : 'Upcoming';
          if (group !== next) { group = next; body.append(el('h3', { class:'work-group '+(group==='Overdue'?'overdue':''),text:group })); }
        }
        const p = proposition(item.proposition_id);
        const label = el('a', { class:'work-link', href:item.url, onclick:(e)=>{
          if(item.proposition_id===state.open&&!e.ctrlKey&&!e.metaKey&&!e.shiftKey&&!e.altKey&&e.button===0){e.preventDefault();location.hash=new URL(item.url,location.origin).hash;}
        } },el('span',{class:'work-title'},inline(item.title,byHandle,proposition)),
          el('span',{class:'work-context',text:p?.title||'Workspace'}),
          item.due_date?el('time',{datetime:item.due_date,class:'work-due'+(item.due_date<today?' overdue':''),text:item.due_date===today?'Today':item.due_date}):el('span',{class:'work-due',text:'No date'}),
          el('span',{class:'work-arrow','aria-hidden':true,text:'↗'}));
        body.append(el('article',{class:'work-item'},label,item.text?el('p',{},inline(item.text,byHandle,proposition)):null));
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
