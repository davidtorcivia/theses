import { el, clear, say, ask } from './dom.js';
import { state, canEdit, apply, emit } from './state.js';
import { newKey, send } from './net.js';
import * as api from './api.js';

const entries=new Map();
const stamp=ms=>ms===null?'Untimed':Math.floor(ms/60000)+':'+String(Math.floor(ms/1000)%60).padStart(2,'0');
export function transcriptSection(file){
  let entry=entries.get(file.id);if(entry?.dataset.version===String(file.metadata_version))return entry;
  if(entry)entry.open=false;
  const body=el('div');entry=el('details',{class:'transcript'},el('summary',{text:'Transcript'}),body);entries.set(file.id,entry);
  entry.dataset.version=String(file.metadata_version);
  let generation=0,poll;
  const load=async()=>{
    const request=++generation;clearTimeout(poll);clear(body).append(el('p',{role:'status',text:'Loading transcript…'}));
    try{
      const [{transcript,local_transcription},{jobs}]=await Promise.all([api.get(`/files/${file.id}/transcript`),api.get(`/files/${file.id}/transcription-jobs`)]);
      if(request!==generation||!entry.open)return;clear(body);
      const current=jobs[0];
      if(current){body.append(el('p',{role:'status',class:'dim',text:'Local transcription: '+current.state+(current.error?' · '+current.error:'')}));if(['queued','running'].includes(current.state))poll=setTimeout(()=>{if(entry.isConnected&&entry.open)load();},5000);}
      const actions=el('div',{class:'acts'});
      if(canEdit()){
        const upload=el('input',{type:'file',accept:'.txt,.srt,.vtt','aria-label':'Import transcript'});
        upload.onchange=async()=>{const selected=upload.files[0];if(!selected)return;if(selected.size>4*1024*1024){say('Transcript must be smaller than 4 MiB.');return;}if(transcript.version&&!await ask('Replace this transcript?','The current transcript and speaker edits will be replaced.','Replace'))return;upload.disabled=true;
          try{await api.replace(`/files/${file.id}/transcript`,{text:await selected.text(),format:selected.name.split('.').pop().toLowerCase(),version:transcript.version},{'Idempotency-Key':newKey()});await load();}catch(err){say(err.message);upload.disabled=false;}
        };
        body.append(el('label',{class:'transcript-import'},'Import TXT, SRT, or VTT',upload));
        const stereo=el('input',{type:'checkbox'});
        if(local_transcription){body.append(el('label',{class:'check-label'},stereo,'Label speakers by stereo channel'),el('p',{class:'dim',text:'Use channel labels only when each host has a separate left/right channel. Mixed or mono recordings need manual labels. Audio is sent to the Whisper service configured by your server operator. No paid transcription API is required.'}));
          actions.append(el('button',{type:'button',class:'act',text:'Transcribe locally',disabled:jobs.some(j=>['queued','running'].includes(j.state)),onclick:async(e)=>{
            const button=e.currentTarget;
            if(transcript.version&&!await ask('Regenerate transcript?','The new transcript will replace this version only if it has not changed while processing.','Transcribe'))return;
            button.disabled=true;button.textContent='Queueing…';
            try{await api.post(`/files/${file.id}/transcription-jobs`,{stereo:stereo.checked},{'Idempotency-Key':newKey()});await load();}catch(err){say(err.message);button.disabled=false;button.textContent='Transcribe locally';}
          }}));
        }else body.append(el('p',{class:'dim',text:'Automatic transcription needs a local Whisper service configured by the server operator. Transcript import is ready to use.'}));
      }
      actions.append(el('button',{class:'lnk',type:'button',text:'Refresh transcript',onclick:load}));
      if(transcript.segments.length)actions.append(el('a',{class:'lnk',href:`/app/files/${file.id}/transcript/export?format=txt`,text:'Export TXT'}),transcript.segments.every(s=>s.start_ms!==null)?el('a',{class:'lnk',href:`/app/files/${file.id}/transcript/export?format=vtt`,text:'Export VTT'}):null);
      body.append(actions);
      if(!transcript.segments.length){body.append(el('p',{class:'dim',text:'Import a transcript or transcribe the recording. Search passages and click timestamps to seek the audio.'}));return;}
      const query=el('input',{type:'search',placeholder:'Search this transcript','aria-label':'Search transcript'});
      const list=el('div',{class:'transcript-segments'});let page=0;
      const draw=()=>{
        clear(list);const q=query.value.toLowerCase();const found=transcript.segments.map((s,i)=>({...s,index:i})).filter(s=>(s.speaker+' '+s.text).toLowerCase().includes(q));
        const last=Math.max(0,Math.ceil(found.length/50)-1);page=Math.min(page,last);
        list.append(el('p',{class:'dim',text:found.length+' passages · page '+(page+1)+' of '+(last+1)}));
        for(const seg of found.slice(page*50,page*50+50)){
          const section=el('article',{class:'transcript-segment'},el('button',{class:'lnk',type:'button',text:stamp(seg.start_ms),disabled:seg.start_ms===null,onclick:()=>{const player=document.querySelector('#drawer audio,#drawer video');if(player){player.currentTime=seg.start_ms/1000;player.focus();}else say('Open the recording player to seek.');}}),seg.speaker?el('strong',{text:seg.speaker}):null,el('p',{text:seg.text}));
          if(canEdit())section.append(el('div',{class:'acts'},el('button',{type:'button',class:'lnk',text:'Edit passage / speaker',onclick:()=>edit(seg,transcript,file,load)}),
            seg.start_ms!==null?el('button',{type:'button',class:'lnk',text:'Add as comment',onclick:async(e)=>{const button=e.currentTarget;button.disabled=true;try{const answer=await api.post(`/files/${file.id}/comments`,{position_ms:seg.start_ms,body_md:(seg.speaker?seg.speaker+': ':'')+seg.text},{'Idempotency-Key':newKey()});apply(answer.event);say('Timestamped comment added');}catch(err){say(err.message);button.disabled=false;}}}):null,
            state.columns.length?el('button',{type:'button',class:'lnk',text:'Create task',onclick:async(e)=>{const button=e.currentTarget;button.disabled=true;try{await send('card.create',{column:state.columns[0].id,title:`Review ${file.name} ${stamp(seg.start_ms)}: ${seg.text}`.slice(0,450),assignees:[state.me]});say('Task added to the first board column');}catch(err){say(err.message);button.disabled=false;}}}):null));
          list.append(section);
        }
        list.append(el('div',{class:'acts'},el('button',{class:'lnk',type:'button',text:'Previous passages',disabled:page===0,onclick:()=>{page--;draw();}}),el('button',{class:'lnk',type:'button',text:'Next passages',disabled:page===last,onclick:()=>{page++;draw();}})));
      };
      query.oninput=()=>{page=0;draw();};body.append(query,list);draw();
    }catch(err){if(request!==generation)return;clear(body).append(el('p',{role:'status',text:err.message}),el('button',{class:'lnk',type:'button',text:'Retry transcript',onclick:load}));}
  };
  entry.addEventListener('toggle',()=>{if(entry.open)load();else{generation++;clearTimeout(poll);}});return entry;
}
function edit(segment,transcript,file,refresh){
  const speaker=el('input',{'aria-label':'Speaker label',maxlength:100,value:segment.speaker});const text=el('textarea',{rows:8,'aria-label':'Transcript passage',required:true});text.value=segment.text;
  const all=el('input',{type:'checkbox'});const status=el('p',{role:'status'});const save=el('button',{type:'submit',class:'act',text:'Save passage'});
  const attempt=api.mutation();
  const close=el('button',{type:'button',class:'lnk',text:'Cancel'});
  const form=el('form',{},el('h3',{text:'Edit transcript passage'}),el('label',{},'Speaker',speaker),el('label',{class:'check-label'},all,'Rename this speaker throughout the transcript'),text,status,el('div',{class:'acts'},close,save));
  const dialog=el('dialog',{class:'workflow-dialog','aria-label':'Edit transcript passage'},form);const back=document.activeElement;
  const dirty=()=>text.value!==segment.text||speaker.value!==segment.speaker;const dismiss=async()=>{if(!save.disabled&&(!dirty()||await ask('Discard passage changes?','Changes have not been saved.','Discard')))dialog.close();};close.onclick=dismiss;dialog.addEventListener('cancel',e=>{e.preventDefault();dismiss();});
  const leaving=e=>{if(dirty()){e.preventDefault();e.returnValue='';}};window.addEventListener('beforeunload',leaving);dialog.addEventListener('close',()=>{window.removeEventListener('beforeunload',leaving);dialog.remove();if(back?.isConnected)back.focus();else entries.get(file.id)?.querySelector('summary')?.focus();});
  form.onsubmit=async e=>{e.preventDefault();save.disabled=true;status.textContent='Saving…';speaker.disabled=text.disabled=all.disabled=true;const segments=transcript.segments.map((s,i)=>({...s,speaker:i===segment.index||(all.checked&&s.speaker===segment.speaker)?speaker.value:s.speaker,text:i===segment.index?text.value:s.text}));
    try{await attempt.run('PUT',`/files/${file.id}/transcript`,{segments,version:transcript.version});dialog.close();refresh();}catch(err){status.textContent=err.message;save.disabled=false;speaker.disabled=text.disabled=all.disabled=attempt.pending;save.textContent=attempt.pending?'Retry save':'Save passage';}
  };document.body.append(dialog);dialog.showModal();
}
