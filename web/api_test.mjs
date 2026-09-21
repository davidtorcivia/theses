import assert from 'node:assert/strict';
globalThis.document={querySelector:()=>null};
globalThis.location={href:'https://example.com/show'};
const api=await import('./static/app/api.js');
const requests=[];
let outcome='lost';
globalThis.fetch=async(path,options)=>{
 requests.push({path,...options});
 assert.ok(options.signal instanceof AbortSignal,'network calls have a deadline');
 if(outcome==='lost')throw new TypeError('network lost');
 if(outcome==='html')return new Response('<html>proxy error</html>',{headers:{'Content-Type':'text/html'}});
 if(outcome==='invalid')return new Response('{',{headers:{'Content-Type':'application/json'}});
 if(outcome==='refused')return new Response('{"error":"Changed"}',{status:409,headers:{'Content-Type':'application/json'}});
 return new Response('{"event":{"seq":1}}',{headers:{'Content-Type':'application/json'}});
};
const attempt=api.mutation();
await assert.rejects(attempt.run('POST','/calendar-events',{title:'Original'}),err=>err.status===0&&/may have saved/.test(err.message));
const first=requests.at(-1);
assert.equal(attempt.pending,true);
for(outcome of ['html','invalid'])await assert.rejects(attempt.run('POST','/calendar-events',{title:'Changed while pending'}),err=>err.status===0);
outcome='ok';await attempt.run('POST','/calendar-events',{title:'Changed while pending'});
assert.equal(attempt.pending,false);
for(const request of requests){assert.equal(request.headers['Idempotency-Key'],first.headers['Idempotency-Key']);assert.equal(request.body,first.body);}
outcome='refused';await assert.rejects(attempt.run('PUT','/evidence',{version:1}),err=>err.status===409);assert.equal(attempt.pending,false);
const refusedKey=requests.at(-1).headers['Idempotency-Key'];
outcome='ok';await attempt.run('PUT','/evidence',{version:2});assert.notEqual(requests.at(-1).headers['Idempotency-Key'],refusedKey);
console.log('HTTP uncertainty, retry identity and bounded requests pass');

const nativeTimeout=AbortSignal.timeout;let deadline;
AbortSignal.timeout=ms=>{deadline=ms;return nativeTimeout(ms);};
try{await api.post('/drive/import',{});assert.ok(deadline>6*60*60*1000,'Drive request outlasts the server copy deadline');}
finally{AbortSignal.timeout=nativeTimeout;}

const timers=new Map();let tick=0,xhr;
const originalSetTimeout=globalThis.setTimeout,originalClearTimeout=globalThis.clearTimeout;
globalThis.setTimeout=(fn,ms)=>{assert.equal(ms,120000);timers.set(++tick,fn);return tick;};
globalThis.clearTimeout=id=>timers.delete(id);
class UploadXHR extends EventTarget{
 constructor(){super();xhr=this;this.upload=new EventTarget();}
 open(){} setRequestHeader(){} send(){}
 abort(){this.dispatchEvent(new Event('abort'));this.dispatchEvent(new Event('loadend'));}
 getResponseHeader(){return 'etag';}
}
globalThis.XMLHttpRequest=UploadXHR;
try{
 const stalled=api.put('https://bucket.example/upload',{},new Blob(['bytes']));
 const firstTimer=tick;xhr.upload.dispatchEvent(new Event('progress'));
 assert.equal(timers.has(firstTimer),false,'progress renews the stall deadline');
 timers.get(tick)();await assert.rejects(stalled,/stopped making progress/);assert.equal(timers.size,0);
 const success=api.put('https://bucket.example/upload',{},new Blob(['bytes']));xhr.status=200;xhr.dispatchEvent(new Event('load'));xhr.dispatchEvent(new Event('loadend'));
 assert.equal(await success,'etag');assert.equal(timers.size,0);
}finally{globalThis.setTimeout=originalSetTimeout;globalThis.clearTimeout=originalClearTimeout;}
console.log('Upload stall deadlines renew on progress and release after completion');
