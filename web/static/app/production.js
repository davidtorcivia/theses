import { newKey } from './net.js';
import { el, clear, say } from './dom.js';
import { state, canEdit, apply } from './state.js';
import { calendarDay } from './filters.js';
import { propositionURL } from './references.js';
import { openCard } from './drawer.js';
import * as api from './api.js';

let calendar, month, grid, calendarKey = '';
export function productionCalendar() {
  if (!calendar) {
    month = el('input', { type: 'month', value: calendarDay(state.timezone).slice(0,7), 'aria-label': 'Production month', onchange: drawCalendar });
    grid = el('div');
    calendar = el('details', { class: 'connected production-calendar' }, el('summary', { text: 'Production calendar' }), month, grid);
  }
  const key = JSON.stringify(state.props);
  if (calendarKey !== key) { calendarKey = key; drawCalendar(); }
  return calendar;
}
function drawCalendar() {
  if (!/^\d{4}-\d{2}$/.test(month.value)) return;
  const [year,m] = month.value.split('-').map(Number);
  const count = new Date(Date.UTC(year,m,0)).getUTCDate();
  const offset = new Date(Date.UTC(year,m-1,1)).getUTCDay();
  const dates = state.props.filter((p) => p.kind !== 'show' && !p.archived_at && p.target_date?.startsWith(month.value));
  const body = el('tbody');
  for (let start=0; start<count+offset; start+=7) {
    const row=el('tr');
    for (let i=0; i<7; i++) {
      const day=start+i-offset+1;
      const cell=el('td');
      if (day>0 && day<=count) {
        const date=month.value+'-'+String(day).padStart(2,'0');
        cell.append(el('time', { datetime: date, text: day }));
        for (const p of dates.filter((p)=>p.target_date===date)) cell.append(el('a', { href: propositionURL(p), text: p.title+' · '+p.status }));
      }
      row.append(cell);
    }
    body.append(row);
  }
  clear(grid).append(el('table', {}, el('caption', { text: 'Target release dates · '+month.value }),
    el('thead', {}, el('tr', {}, ['Sun','Mon','Tue','Wed','Thu','Fri','Sat'].map((d)=>el('th', { scope: 'col', text: d })))),body));
  const unscheduled=state.props.filter((p)=>p.kind!=='show'&&!p.archived_at&&!p.target_date);
  if (unscheduled.length) grid.append(el('p', { text: 'Unscheduled: ' }), el('ul', {},unscheduled.map((p)=>el('li', {},el('a', { href: propositionURL(p)+'/settings',text:p.title })))));
}

const readiness = new Map();
export function productionReadiness(p) {
  let entry=readiness.get(p.id);
  if (!entry) {
    const body=el('div');
    const details=el('details', { class:'connected' },el('summary',{text:'Production readiness'}),body);
    entry={body,details,seq:-1,request:0};readiness.set(p.id,entry);
    details.addEventListener('toggle',()=>{if(details.open) readReadiness(p.id,entry);});
  }
  if(entry.details.open && entry.seq!==state.seq) {entry.seq=state.seq;readReadiness(p.id,entry);}
  return entry.details;
}
async function readReadiness(id,entry) {
  const request=++entry.request;
  entry.seq=state.seq;
  try {
    const answer=await api.get('/production?proposition='+id);
    if(request!==entry.request)return;
    const p=state.props.find(p=>p.id===id);if(!p)return;
    const focused = entry.body.contains(document.activeElement);
    clear(entry.body);
    const card=answer.card;
    const list=card?.checklist||[];
    const done=list.filter(i=>i.done).length;
    entry.body.append(el('p',{text:card?`${done} of ${list.length} production checks complete.`:'Start with the review, record, edit, and publish checklist.'}));
    if(!p.target_date)entry.body.append(el('p',{text:'No target release date. Set one in proposition settings.'}));
    if(!answer.recordings)entry.body.append(el('p',{text:'No recording uploaded yet.'}));
    entry.body.append(el('p',{class:'dim',text:'Readiness is a checklist. Publishing remains a separate, manual action.'}));
    if(card)entry.body.append(el('button',{type:'button',class:'lnk',text:'Open production checklist',onclick:()=>openCard(card.id)}));
    else if(canEdit())entry.body.append(el('button',{type:'button',class:'lnk',text:'Create production checklist',onclick:async(e)=>{
      const button=e.currentTarget;button.disabled=true;
      try {const result=await api.post(`/propositions/${id}/production-template`,{}, {'Idempotency-Key':newKey()});apply(result.event);openCard(result.card.id);readReadiness(id,entry);}catch(err){say(err.message);button.disabled=false;}
    }}));
    if (focused) entry.body.querySelector('button')?.focus();
  }catch(err){if(request!==entry.request)return;const focused=entry.body.contains(document.activeElement);clear(entry.body).append(el('p',{text:err.message}),el('button',{type:'button',class:'lnk',text:'Retry readiness',onclick:()=>readReadiness(id,entry)}));if(focused)entry.body.querySelector('button')?.focus();}
}
