import { el, clear, inline, say, ask } from './dom.js';
import { state, canEdit, proposition, byHandle, apply, material } from './state.js';
import { newKey } from './net.js';
import * as api from './api.js';

function modal(title,wide=false) {
  const back=document.activeElement;
  const body=el('div',{class:'workflow-body'});
  const close=el('button',{type:'button',class:'lnk',text:'Close',onclick:()=>dialog.close()});
  const dialog=el('dialog',{class:'workflow-dialog'+(wide?' recording-view':''),'aria-label':title},el('header',{},el('h2',{text:title}),close),body);
  dialog.addEventListener('close',()=>{dialog.remove();if(back?.isConnected)back.focus();});
  document.body.append(dialog);dialog.showModal();body.append(el('p',{role:'status',text:'Loading…'}));
  return {dialog,body};
}
export async function recordingView(doc) {
  const {dialog,body}=modal('Recording view · '+doc.name,true);
  let timer,start=0,elapsed=0,readingSize='medium',readingPace=150,cues='',pinning=false;
  const dismiss=async()=>{if(!pinning&&(!cues||await ask('Discard recording cues?','Pin the saved script to keep these cues.','Discard')))dialog.close();};
  dialog.querySelector('header button').onclick=dismiss;dialog.addEventListener('cancel',e=>{e.preventDefault();dismiss();});
  const leaving=e=>{if(cues){e.preventDefault();e.returnValue='';}};window.addEventListener('beforeunload',leaving);dialog.addEventListener('close',()=>window.removeEventListener('beforeunload',leaving));
  dialog.addEventListener('close',()=>clearInterval(timer));
  const draw=async()=>{
    try{
      const {snapshots}=await api.get(`/documents/${doc.id}/snapshots`);if(!dialog.open)return;
      clear(body);
      const stage=el('div',{class:'recording-stage size-medium'});
      const notes=el('textarea',{rows:3,placeholder:'Pronunciations, pauses, handoffs, or recording cues','aria-label':'Recording cues'});
      notes.value=cues;notes.oninput=()=>{cues=notes.value;};
      const pin=el('button',{type:'button',class:'act',text:'Pin saved script',onclick:async()=>{
        pinning=true;pin.disabled=true;notes.disabled=true;pin.textContent='Pinning…';
        try{await api.post(`/documents/${doc.id}/snapshots`,{cues},{'Idempotency-Key':newKey()});cues='';await draw();}catch(err){say(err.message);pin.disabled=false;notes.disabled=false;pin.textContent='Pin saved script';}finally{pinning=false;}
      }});
      const choose=el('select',{'aria-label':'Pinned script'},snapshots.map(s=>el('option',{value:s.id,text:'#'+s.id+' · '+new Date(s.created_at*1000).toLocaleString()})));
      const size=el('select',{'aria-label':'Reading text size',onchange:()=>{readingSize=size.value;stage.className='recording-stage size-'+size.value;}},['small','medium','large'].map(s=>el('option',{value:s,text:s+' type',selected:s==='medium'})));
      const pace=el('input',{type:'number',min:80,max:250,value:readingPace,'aria-label':'Words per minute'});
      const estimate=el('span',{class:'dim'});
      const clock=el('output',{text:'00:00','aria-label':'Elapsed recording time'});

      const run=el('button',{type:'button',class:'lnk',text:'Start timer',onclick:()=>{
        if(start){elapsed+=performance.now()-start;start=0;run.textContent='Resume timer';}else{start=performance.now();run.textContent='Pause timer';}
      }});
      const updateClock=()=>{const sec=Math.floor((elapsed+(start?performance.now()-start:0))/1000);clock.textContent=String(Math.floor(sec/60)).padStart(2,'0')+':'+String(sec%60).padStart(2,'0');};
      clearInterval(timer);timer=setInterval(updateClock,500);
      const outline=el('nav',{'aria-label':'Recording outline',class:'recording-outline'});
      const show=()=>{
        clear(stage);clear(outline);const snapshot=snapshots.find(s=>s.id===Number(choose.value));if(!snapshot)return;
        if(snapshot.cues)stage.append(el('aside',{class:'recording-cues',text:snapshot.cues}));
        snapshot.markdown.split(/\n\s*\n/).forEach((text,index)=>{
          const heading=text.match(/^(#{1,6})\s+(.+)$/);
          const node=heading?el('h'+Math.min(heading[1].length+1,6),{id:'recording-'+index,text:heading[2]}):el('p',{},inline(text,byHandle,proposition));
          stage.append(node);if(heading)outline.append(el('button',{type:'button',class:'lnk',text:heading[2],onclick:()=>node.scrollIntoView({block:'start'})}));
        });
        const words=snapshot.markdown.trim().split(/\s+/).filter(Boolean).length;
        estimate.textContent=words+' words · about '+Math.ceil(words/Math.max(80,Math.min(250,Number(pace.value)||150)))+' min';
      };
      size.value=readingSize;stage.className='recording-stage size-'+readingSize;run.textContent=start?'Pause timer':elapsed?'Resume timer':'Start timer';updateClock();
      choose.onchange=show;pace.oninput=()=>{readingPace=Number(pace.value)||150;show();};
      body.append(canEdit()?el('details',{class:'recording-pin'},el('summary',{text:'Pin a script for this session'}),el('p',{text:'Pin uses the last server-saved script. Finish syncing edits first. Pinned text and cues stay fixed when the document changes.'}),notes,pin):null,
        el('div',{class:'recording-controls'},choose,size,el('label',{},'Words/min ',pace),estimate,clock,run),outline,stage);
      if(!snapshots.length)stage.append(el('p',{text:canEdit()?'Pin the saved script to prepare your recording session.':'No pinned scripts yet. An editor can pin a script for the recording session.'}));else show();
    }catch(err){clear(body).append(el('p',{role:'status',text:err.message}),el('button',{type:'button',class:'lnk',text:'Retry',onclick:draw}));}
  };await draw();
}
export async function reviewTarget(kind,id) {
  const {dialog,body}=modal(kind==='proposition'?'Episode review history':kind==='document'?'Script review':'Recording review');
  const prop=state.open;
  let dirty=false,saving=false;const drafts=new Map();
  const dismiss=async()=>{if(!saving&&(!dirty||await ask('Discard review feedback?','Your feedback has not been saved.','Discard')))dialog.close();};
  dialog.querySelector('header button').onclick=dismiss;dialog.addEventListener('cancel',e=>{e.preventDefault();dismiss();});
  const leaving=e=>{if(dirty){e.preventDefault();e.returnValue='';}};window.addEventListener('beforeunload',leaving);dialog.addEventListener('close',()=>window.removeEventListener('beforeunload',leaving));
  const draw=async()=>{
    try{
      const {reviews}=await api.get('/reviews?proposition='+prop);if(!dialog.open)return;clear(body);
      const rows=reviews.filter(r=>r[kind+'_id']===id);
      body.append(el('p',{text:'A decision applies only to the requested version. New script edits or a replacement audio file require a new review.'}));
      for(const row of rows){
        const section=el('article',{class:'review-item'},el('strong',{text:(kind==='proposition'?(row.document_id?'Script #'+row.document_id:'Audio #'+row.file_id)+' · ':'')+(row.stale?'Superseded · ':'')+row.state.replaceAll('_',' ')}),el('p',{class:'dim',text:(state.users.get(row.reviewer_id)?.name||'Former member')+' · '+new Date(row.created_at*1000).toLocaleDateString()}),row.note?el('p',{text:row.note}):null);
        if(row.snapshot_id)section.append(el('button',{type:'button',class:'lnk',text:'View requested script #'+row.snapshot_id,onclick:async()=>{
          try{const {snapshot:pinned}=await api.get('/snapshots/'+row.snapshot_id);const view=modal('Requested script #'+pinned.id,true);clear(view.body).append(el('pre',{class:'review-script',text:pinned.markdown}));}catch(err){say(err.message);}
        }}));
        if(!row.stale&&row.reviewer_id===state.me&&canEdit()){
          const note=el('textarea',{rows:3,'aria-label':'Review feedback',placeholder:'Feedback or decision notes'});note.value=drafts.get(row.id)??row.note??'';note.oninput=()=>{if(note.value===(row.note||''))drafts.delete(row.id);else drafts.set(row.id,note.value);dirty=drafts.size>0;};
          section.append(note,el('div',{class:'acts'},['changes_requested','approved'].map(value=>el('button',{type:'button',class:'act',text:value==='approved'?'Approve version':'Request changes',onclick:async(e)=>{
            if(saving)return;const button=e.currentTarget;button.disabled=true;note.disabled=true;saving=true;
            try{const answer=await api.patch('/reviews/'+row.id,{state:value,note:note.value,version:row.version});apply(answer.event);drafts.delete(row.id);dirty=drafts.size>0;await draw();}catch(err){say(err.message);button.disabled=false;note.disabled=false;}finally{saving=false;}
          }}))));
        }body.append(section);
      }
      if(!rows.length)body.append(el('p',{class:'dim',text:'No reviews requested for this item.'}));
      if(canEdit()&&kind!=='proposition'){
        const p=proposition(prop);
        const select=el('select',{'aria-label':'Reviewer'},[...state.users.values()].filter(u=>['owner','editor','researcher'].includes(u.role)&&(u.role==='owner'||p?.members?.includes(u.id))).map(u=>el('option',{value:u.id,text:u.name})));
        const request=el('button',{type:'button',class:'act',text:'Request review of current version',onclick:async()=>{if(saving)return;request.disabled=true;saving=true;try{const answer=await api.post('/reviews',{[kind+'_id']:id,reviewer_id:Number(select.value)},{'Idempotency-Key':newKey()});apply(answer.event);await draw();}catch(err){say(err.message);request.disabled=false;}finally{saving=false;}}});
        body.append(el('div',{class:'review-request'},el('label',{},'Reviewer',select),request));
      }
    }catch(err){clear(body).append(el('p',{text:err.message}),el('button',{class:'lnk',type:'button',text:'Retry',onclick:draw}));}
  };await draw();
}
const research=new Map();
export function researchSection(p) {
  let entry=research.get(p.id);if(entry)return entry;
  const body=el('div');
  entry=el('details',{class:'connected research'},el('summary',{text:'Research & references'}),body);
  let generation=0;
  const draw=async()=>{
    const request=++generation;
    clear(body).append(el('p',{role:'status',text:'Loading references…'}));
    try{
      const {evidence}=await api.get('/evidence?proposition='+p.id);if(!entry.open||request!==generation)return;clear(body);
      body.append(el('div',{class:'acts'},el('button',{class:'lnk',type:'button',text:'Review history',onclick:()=>reviewTarget('proposition',p.id)}),canEdit()?el('button',{type:'button',class:'act',text:'Add evidence',onclick:()=>editEvidence(p,{},draw)}):null,
        el('a',{class:'lnk',href:`/app/evidence/export?proposition=${p.id}&format=md`,text:'Export Markdown'}),el('a',{class:'lnk',href:`/app/evidence/export?proposition=${p.id}&format=ris`,text:'Export RIS'}),el('button',{class:'lnk',type:'button',text:'Refresh',onclick:draw})));
      if(!evidence.length)body.append(el('p',{class:'dim',text:'Save quotations, page numbers or timestamps, and the claims they support. RIS exports open in reference managers such as Zotero.'}));
      for(const e of evidence)body.append(el('article',{id:'evidence-'+e.id,class:'evidence-item'},el('strong',{text:e.title}),el('span',{class:'evidence-status',text:e.verified?'Verified':'Needs checking'}),
        el('p',{class:'dim',text:[e.author,e.year,e.locator].filter(Boolean).join(' · ')}),e.quotation?el('blockquote',{text:e.quotation}):null,e.interpretation?el('p',{},el('strong',{text:'Interpretation: '}),e.interpretation):null,e.claim?el('p',{},el('strong',{text:'Supports: '}),e.claim):null,
        el('div',{class:'acts'},e.url?el('a',{href:e.url,target:'_blank',rel:'noopener noreferrer',class:'lnk',text:'Source'}):null,e.block_id?el('a',{href:'#block-'+e.block_id,class:'lnk',text:'Script passage'}):null,
        el('button',{class:'lnk',type:'button',text:'Copy reference',onclick:async()=>{try{await navigator.clipboard.writeText(`[${e.title.replace(/[\[\]]/g,'')}](${location.origin}/p/${p.id}#evidence-${e.id})`);say('Reference copied');}catch{say('Clipboard access failed.');}}}),canEdit()?el('button',{class:'lnk',type:'button',text:'Edit evidence',onclick:()=>editEvidence(p,e,draw)}):null)));
    }catch(err){clear(body).append(el('p',{text:err.message}),el('button',{class:'lnk',type:'button',text:'Retry',onclick:draw}));}
  };
  entry.load=draw;entry.addEventListener('toggle',()=>{if(entry.open)draw();});research.set(p.id,entry);return entry;
}
async function editEvidence(p,e,refresh){
  const {dialog,body}=modal(e.id?'Edit evidence':'Add evidence');
 try{await material();}catch(err){clear(body).append(el('p',{text:err.message}));return;}if(!dialog.open)return;clear(body);
  const fields={};
  const form=el('form',{class:'evidence-form'});
  for(const [key,label,multi] of [['title','Reference title'],['author','Author'],['year','Year'],['url','Source URL'],['quotation','Exact quotation',true],['locator','Page, section, or timestamp'],['interpretation','Your interpretation',true],['claim','Claim this supports',true]]){
    const input=el(multi?'textarea':'input',{rows:3,required:key==='title',type:key==='url'?'url':null});input.value=e[key]||'';fields[key]=input;form.append(el('label',{},el('span',{text:label}),input));
  }
  for(const [key,label,items] of [['link_id','Linked source',state.links.filter(l=>l.proposition_id===p.id).map(l=>[l.id,l.title||l.url])],['file_id','Source file',state.files.filter(f=>f.proposition_id===p.id).map(f=>[f.id,f.name])],['block_id','Script passage',state.documents.flatMap(d=>(d.blocks||[]).map(b=>[b.id,d.name+' · '+b.text.slice(0,75)]))]]){
    const input=el('select',{},el('option',{value:'',text:'None'}),items.map(([id,title])=>el('option',{value:id,text:title,selected:e[key]===id})));if(e[key]&&!items.some(([id])=>id===e[key]))input.append(el('option',{value:e[key],text:'Current #'+e[key],selected:true}));fields[key]=input;form.append(el('label',{},el('span',{text:label}),input));
  }
  const verified=el('input',{type:'checkbox',checked:e.verified});
  const status=el('p',{role:'status'});const save=el('button',{class:'act',type:'submit',text:'Save evidence'});
  form.append(el('label',{class:'check-label'},verified,'Source checked and claim verified'),status,save);body.append(form);
  if(e.id&&state.can.delete)form.append(el('button',{type:'button',class:'lnk',text:'Delete evidence',onclick:async()=>{if(save.disabled||!await ask('Delete this evidence?','This removes the saved reference and its quotation.','Delete'))return;save.disabled=true;try{const answer=await api.del('/evidence/'+e.id+'?version='+e.version);apply(answer.event);dialog.close();refresh();}catch(err){status.textContent=err.message;save.disabled=false;}}}));
  let dirty=false,saved=false;form.addEventListener('input',event=>{dirty=true;if(event.target!==verified)verified.checked=false;});
  const leaving=ev=>{if(dirty&&!saved){ev.preventDefault();ev.returnValue='';}};window.addEventListener('beforeunload',leaving);dialog.addEventListener('close',()=>window.removeEventListener('beforeunload',leaving));
  const dismiss=async()=>{if(!save.disabled&&(!dirty||await ask('Discard evidence changes?','Your text has not been saved.','Discard')))dialog.close();};dialog.querySelector('header button').onclick=dismiss;dialog.addEventListener('cancel',ev=>{ev.preventDefault();dismiss();});
  let attempt;
  form.onsubmit=async ev=>{ev.preventDefault();save.disabled=true;status.textContent='Saving…';
    const values=Object.fromEntries(Object.entries(fields).map(([k,v])=>[k,k.endsWith('_id')?(Number(v.value)||null):v.value]));
    attempt||={key:newKey(),value:{...e,...values,proposition_id:p.id,verified:verified.checked,version:e.version||0}};
    for(const input of form.querySelectorAll('input,textarea,select'))input.disabled=true;
    try{const answer=await api.replace('/evidence',attempt.value,{'Idempotency-Key':attempt.key});saved=true;apply(answer.event);dialog.close();refresh();}
    catch(err){status.textContent=err.message;if(err.status&&err.status<500){attempt=null;for(const input of form.querySelectorAll('input,textarea,select'))input.disabled=false;}else status.textContent+=' Retry sends the same evidence once.';}finally{save.disabled=false;}
  };
}

export async function openEvidence(id){
 const section=researchSection(proposition(state.open));section.open=true;await section.load();
 const node=document.getElementById('evidence-'+id);if(node){node.tabIndex=-1;node.scrollIntoView({block:'center'});node.focus({preventScroll:true});}else say('This reference is unavailable.');
}
