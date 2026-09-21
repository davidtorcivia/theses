import { newKey } from './net.js';
import { el, clear, say } from './dom.js';
import { state, canEdit, apply } from './state.js';
import { monthDate, moveMonth, usHolidays } from './calendar.js';
import { calendarDay } from './filters.js';
import { propositionURL } from './references.js';
import { openCard } from './drawer.js';
import * as api from './api.js';

let calendar, month, monthTitle, holidayToggle, previous, next, grid, calendarKey = '', plans = [], planSeq = -1, calendarRequest = 0;
export function productionCalendar() {
  if (!calendar) {
    month = el('input', { type: 'month', min:'2000-01', max:'2100-12', value: calendarDay(state.timezone).slice(0,7), 'aria-label': 'Production month', onchange: drawCalendar });
    monthTitle=el('h3',{class:'calendar-month-title','aria-live':'polite'});
    holidayToggle=el('input',{type:'checkbox',checked:true,onchange:drawCalendar});
    grid = el('div', { class: 'calendar-content' });
    const move = step => {month.value=moveMonth(month.value,step);drawCalendar();};
    previous=el('button',{type:'button',class:'calendar-step','aria-label':'Previous month',text:'←',onclick:()=>move(-1)});
    next=el('button',{type:'button',class:'calendar-step','aria-label':'Next month',text:'→',onclick:()=>move(1)});
    calendar = el('details', { class: 'connected production-calendar' },
      el('summary', {}, el('span', { class: 'section-eyebrow', text: 'THE SCHEDULE' }), el('span', { text: 'Production calendar' })),
      el('div',{class:'calendar-toolbar'},el('div',{class:'calendar-heading'},monthTitle,el('p',{class:'dim',text:'Make room for your next episode.'})),
        el('div',{class:'calendar-navigation'},previous,el('label',{class:'calendar-jump'},el('span',{text:'Jump to month'}),month),next,
          el('button',{type:'button',class:'lnk',text:'Today',onclick:()=>{month.value=calendarDay(state.timezone).slice(0,7);drawCalendar();}}))),
      el('div',{class:'calendar-options'},el('div',{class:'calendar-legend'},['Record','Edit','Release'].map(label=>el('span',{class:'milestone '+label.toLowerCase(),text:label}))),
        el('label',{class:'calendar-holidays-toggle'},holidayToggle,'U.S. holidays & observances'),el('a',{class:'lnk',href:'/profile#calendar',text:'Calendar sync'})),grid);
    calendar.addEventListener('toggle',()=>{if(calendar.open)loadPlans();});
  }
  const key = JSON.stringify(state.props);
  if (calendarKey !== key) { calendarKey = key; drawCalendar(); }
  if(calendar.open && planSeq!==state.seq)loadPlans();
  return calendar;
}
async function loadPlans() {
  planSeq=state.seq;const request=++calendarRequest;
  try {const result=await api.get('/production-plans');if(request!==calendarRequest)return;plans=result.plans||[];drawCalendar();}
  catch(err){if(request===calendarRequest){planSeq=-1;grid.append(el('p',{role:'status',text:err.message}),el('button',{class:'lnk',type:'button',text:'Retry schedule',onclick:loadPlans}));}}
}
function drawCalendar() {
  if (!monthDate(month.value)) month.value=calendarDay(state.timezone).slice(0,7);
  clear(monthTitle).append(el('span',{text:monthDate(month.value).toLocaleDateString(undefined,{month:'long',timeZone:'UTC'})}),' ',el('span',{class:'calendar-year',text:month.value.slice(0,4)}));
  previous.disabled=month.value==='2000-01';next.disabled=month.value==='2100-12';
  const [year,m] = month.value.split('-').map(Number);
  const count = new Date(Date.UTC(year,m,0)).getUTCDate();
  const offset = new Date(Date.UTC(year,m-1,1)).getUTCDay();
  const today=calendarDay(state.timezone);
  const holidays=holidayToggle.checked?usHolidays(year):new Map();
  const episodes=state.props.filter(p=>p.kind!=='show'&&!p.archived_at);
  const byPlan=new Map(plans.map(p=>[p.proposition_id,p]));
  const dated=new Map();
  for(const p of episodes){const plan=byPlan.get(p.id)||{};for(const [kind,date] of [['record',plan.record_date],['edit',plan.edit_date],['release',p.target_date]]){
    if(!date?.startsWith(month.value))continue;
    if(!dated.has(date))dated.set(date,[]);dated.get(date).push({p,kind});
  }}
  const body = el('tbody');
  for (let start=0; start<count+offset; start+=7) {
    const row=el('tr');
    for (let i=0; i<7; i++) {
      const day=start+i-offset+1;
      const cell=el('td');
      if (day>0 && day<=count) {
        const date=month.value+'-'+String(day).padStart(2,'0');
        cell.className=[date===today?'calendar-today':'',i===0||i===6?'calendar-weekend':''].filter(Boolean).join(' ');
        cell.append(el('time', { datetime: date, text: day, 'aria-label':new Date(date+'T12:00:00Z').toLocaleDateString(undefined,{weekday:'long',month:'long',day:'numeric',year:'numeric',timeZone:'UTC'}), 'aria-current':date===today?'date':null }));
        for(const name of holidays.get(date)||[])cell.append(el('span',{class:'calendar-holiday',text:name}));
        for (const {p,kind} of dated.get(date)||[]) cell.append(el('a', { class:'calendar-event '+kind, href: propositionURL(p), title:kind+': '+p.title },el('span',{class:'event-kind',text:kind}),el('span',{text:p.title})));
      } else cell.className='calendar-outside';
      row.append(cell);
    }
    body.append(row);
  }
  const events=[...dated.values()].flat();
  clear(grid).append(el('p',{class:'calendar-count',text:events.length+' milestones this month · '+episodes.length+' active propositions'}),
    el('div',{class:'calendar-scroll'},el('table', {}, el('caption', { class:'sr-only',text:'Production milestones for '+month.value }),
      el('thead', {}, el('tr', {}, ['Sunday','Monday','Tuesday','Wednesday','Thursday','Friday','Saturday'].map(d=>el('th',{scope:'col'},el('abbr',{title:d,text:d.slice(0,3)}),el('span',{class:'weekday-full',text:d}))))),body)));
  const agenda=el('div',{class:'calendar-agenda','aria-label':'Monthly production agenda'},el('h4',{text:'Dates this month'}));
  const agendaDays=new Map([...dated]);
  for(const date of holidays.keys())if(date.startsWith(month.value)&&!agendaDays.has(date))agendaDays.set(date,[]);
  for(const [date,items] of [...agendaDays].sort(([a],[b])=>a.localeCompare(b)))agenda.append(el('section',{},el('time',{datetime:date,text:new Date(date+'T12:00:00Z').toLocaleDateString(undefined,{weekday:'long',month:'long',day:'numeric',timeZone:'UTC'})}), (holidays.get(date)||[]).map(name=>el('p',{class:'calendar-holiday',text:name})),items.map(({p,kind})=>el('a',{class:'calendar-event '+kind,href:propositionURL(p)},el('span',{class:'event-kind',text:kind}),el('span',{text:p.title})))));
  if(!agendaDays.size)agenda.append(el('p',{class:'dim',text:'No milestones this month. Set recording, edit, or release dates on an episode.'}));
  grid.append(agenda);
  const attention=episodes.filter(p=>{const plan=byPlan.get(p.id);return plan?.blocker||!p.target_date||!plan?.next_action||!plan?.owner_id;});
  if(episodes.length)grid.append(el('details',{class:'calendar-attention'},el('summary',{text:'Episode overview · '+attention.length+' need attention'}),
    el('div',{class:'attention-list'},episodes.map(p=>{const plan=byPlan.get(p.id)||{};return el('a',{href:propositionURL(p),class:'attention-item'},
      el('span',{text:p.title+' · '+p.status}),el('span',{class:'dim',text:(state.users.get(plan.owner_id)?.name||'Unassigned')+' · '+(plan.next_action||'Set the next action')}),el('span',{class:plan.blocker?'blocked dim':'dim',text:plan.blocker||(!p.target_date?'Release unscheduled':'Release '+p.target_date)}));}))));
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
    const active=document.activeElement;const cursor=[active?.selectionStart,active?.selectionEnd];
    const focused = entry.body.contains(active);
    const oldPlan=entry.body.querySelector(".production-plan");
    const planForm=oldPlan?.dataset.dirty==='true'?oldPlan:null;
    clear(entry.body);
    const card=answer.card;
    const list=card?.checklist||[];
    const done=list.filter(i=>i.done).length;
    entry.body.append(planForm || planEditor(p,answer.plan||{version:0}));
    entry.body.append(el('p',{text:card?`${done} of ${list.length} production checks complete.`:'Start with the review, record, edit, and publish checklist.'}));
    if(!p.target_date)entry.body.append(el('p',{text:'No target release date. Set one in proposition settings.'}));
    if(!answer.recordings)entry.body.append(el('p',{text:'No recording uploaded yet.'}));
    entry.body.append(el('p',{class:'dim',text:'Readiness is a checklist. Publishing remains a separate, manual action.'}));
    if(card)entry.body.append(el('button',{type:'button',class:'lnk',text:'Open production checklist',onclick:()=>openCard(card.id)}));
    else if(canEdit())entry.body.append(el('button',{type:'button',class:'lnk',text:'Create production checklist',onclick:async(e)=>{
      const button=e.currentTarget;button.disabled=true;
      try {const result=await api.post(`/propositions/${id}/production-template`,{}, {'Idempotency-Key':newKey()});apply(result.event);openCard(result.card.id);readReadiness(id,entry);}catch(err){say(err.message);button.disabled=false;}
    }}));
    if(focused){if(planForm?.contains(active)){active.focus();if(typeof cursor[0]==='number')active.setSelectionRange(cursor[0],cursor[1]);}else entry.body.querySelector('button')?.focus();}
  }catch(err){if(request!==entry.request)return;const focused=entry.body.contains(document.activeElement);const draft=entry.body.querySelector('.production-plan[data-dirty="true"]');if(!draft)clear(entry.body);entry.body.querySelector('.readiness-error')?.remove();entry.body.append(el('div',{class:'readiness-error'},el('p',{text:err.message}),el('button',{type:'button',class:'lnk',text:'Retry readiness',onclick:()=>readReadiness(id,entry)})));if(focused&&!draft)entry.body.querySelector('button')?.focus();}
}

function planEditor(p,plan) {
  const owner=el('select',{'aria-label':'Production owner'},el('option',{value:'',text:'Unassigned'}),
    [...state.users.values()].filter(u=>u.role==='owner'||p.members?.includes(u.id)).map(u=>el('option',{value:u.id,text:u.name,selected:u.id===plan.owner_id})));
  // Keep a former member visible until the assignment is explicitly changed.
  if(plan.owner_id&&!owner.querySelector(`option[value="${plan.owner_id}"]`))owner.append(el('option',{value:plan.owner_id,text:state.users.get(plan.owner_id)?.name||'Former member',selected:true}));
  const action=el('input',{value:plan.next_action||'',maxlength:500,placeholder:'What moves this episode forward?'});
  const blocker=el('input',{value:plan.blocker||'',maxlength:500,placeholder:'None. Ready to move.'});
  const record=el('input',{type:'date',value:plan.record_date||''});
  const edit=el('input',{type:'date',value:plan.edit_date||''});
  const status=el('p',{role:'status',class:'dim'});
  const save=el('button',{type:'submit',class:'act',text:'Save production plan'});
  let version=plan.version;
  const form=el('form',{class:'production-plan',onsubmit:async(e)=>{
    e.preventDefault();save.disabled=true;for(const input of [owner,action,blocker,record,edit])input.disabled=true;status.textContent='Saving…';
    try{const result=await api.replace(`/propositions/${p.id}/production-plan`,{owner_id:owner.value?Number(owner.value):null,next_action:action.value,blocker:blocker.value,record_date:record.value,edit_date:edit.value,version},{'Idempotency-Key':newKey()});version=result.plan.version;form.dataset.dirty='false';status.textContent='Saved';apply(result.event);}
    catch(err){status.textContent=err.message;if(err.status===409)status.append(el('button',{type:'button',class:'lnk',text:'Load current plan (discard my edits)',onclick:async()=>{try{const {plan}=await api.get(`/propositions/${p.id}/production-plan`);form.replaceWith(planEditor(p,plan));}catch(error){say(error.message);}}}));}finally{save.disabled=false;for(const input of [owner,action,blocker,record,edit])input.disabled=false;}
  }},el('div',{class:'plan-fields'},[['Next action',action],['Responsible owner',owner],['Blocked by',blocker],['Record by',record],['Edit by',edit]].map(([name,input])=>el('label',{},el('span',{text:name}),input))),
  el('p',{class:'dim',text:p.target_date?'Release · '+p.target_date:'Release date is set in proposition settings.'}),canEdit()?save:null,status);
  form.addEventListener('input',()=>form.dataset.dirty='true');
  if(!canEdit())for(const input of [owner,action,blocker,record,edit])input.disabled=true;
  return form;
}
