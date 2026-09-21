import assert from 'node:assert/strict';
import fs from 'node:fs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const fixture=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const browser=await chromium.launch({headless:true,executablePath:process.env.BROWSER_EXECUTABLE||undefined});
try{
 const context=await browser.newContext({viewport:{width:390,height:844},hasTouch:true});await context.addCookies(fixture.cookies);
 const page=await context.newPage();page.setDefaultTimeout(15000);page.on('dialog',dialog=>dialog.accept());
 const errors=[];page.on('pageerror',error=>errors.push(error.message));
 await page.goto(fixture.url+'/p/'+fixture.proposition);await page.locator('#board').waitFor();
 const doc=Number(await page.locator('.dtab.on').getAttribute('data-d'));
 const csrf=await page.locator('meta[name=csrf]').getAttribute('content');
 const request=async(method,path,body)=>{
  const response=await context.request.fetch(fixture.url+'/app'+path,{method,headers:{'X-CSRF-Token':csrf},data:body});assert.equal(response.status(),200,await response.text());return response.json();
 };
 assert.equal(await page.locator('#netbar').getAttribute('role'),'status');
 await page.getByLabel('Document tools',{exact:true}).tap();await page.getByRole('button',{name:'Recording view',exact:true}).tap();
 await page.locator('.recording-pin summary').tap();await page.getByLabel('Recording cues',{exact:true}).fill('Retain this uncertain pin');
 const pinPath=`/documents/${doc}/snapshots`;
 await page.route('**/app'+pinPath,async route=>{if(route.request().method()==='POST'){await route.fetch();await route.abort();}else await route.continue();});
 await page.getByRole('button',{name:'Pin saved script',exact:true}).tap();await page.getByRole('button',{name:'Retry pin',exact:true}).waitFor();
 assert.equal(await page.getByLabel('Recording cues',{exact:true}).isDisabled(),true);
 await page.unroute('**/app'+pinPath);await page.getByRole('button',{name:'Retry pin',exact:true}).tap();
 await page.locator('.recording-cues').waitFor();assert.equal((await request('GET',pinPath)).snapshots.length,1);
 await page.locator('dialog header button').tap();
 await page.getByLabel('Document tools',{exact:true}).tap();await page.getByRole('button',{name:'Review',exact:true}).tap();
 await page.route('**/app/reviews',async route=>{await route.fetch();await route.abort();});
 await page.getByRole('button',{name:'Request review of current version',exact:true}).tap();await page.getByRole('button',{name:'Retry review request',exact:true}).waitFor();
 await page.unroute('**/app/reviews');await page.getByRole('button',{name:'Retry review request',exact:true}).tap();
 await page.locator('.review-item').waitFor();assert.equal((await request('GET','/reviews?proposition='+fixture.proposition)).reviews.length,1);
 await page.locator('dialog header button').tap();
 // Remote deletion must leave an unsaved source draft reachable before and after reload.
 await page.locator('#docmode').tap();await page.locator('#docsrc').fill('Local words that must survive deletion');
 await page.getByText('Draft on this device · Save to publish',{exact:true}).waitFor();
 const other=await context.newPage();await other.goto(fixture.url+'/p/'+fixture.proposition);await other.locator('#board').waitFor();
 await other.evaluate(async id=>{const src=document.querySelector('script[type=module]').src;const {send}=await import(new URL('net.js',src));await send('document.delete',{document:id});},doc);await other.close();
 await page.getByText('Unsaved draft from deleted document #'+doc,{exact:true}).waitFor();
 await page.reload();await page.getByText('Unsaved draft from deleted document #'+doc,{exact:true}).click();
 assert.equal(await page.getByLabel('Unsaved draft from deleted document',{exact:true}).inputValue(),'Local words that must survive deletion');
 await page.locator('#activitytab').tap();await page.getByRole('button',{name:'Recently deleted',exact:true}).tap();
 const trash=page.getByRole('dialog',{name:'Recently deleted',exact:true});await trash.getByRole('button',{name:'Restore',exact:true}).waitFor();
 for(const width of [320,390,768,1024]){await page.setViewportSize({width,height:844});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'Trash overflow '+width);assert.ok((await trash.getByRole('button',{name:'Restore',exact:true}).boundingBox()).height>=40);if(process.env.THESES_SCREENSHOT_DIR&&[320,1024].includes(width))await trash.screenshot({path:process.env.THESES_SCREENSHOT_DIR+'/trash-'+width+'.png'});}
 await trash.getByRole('button',{name:'Restore',exact:true}).click();await trash.getByText('No recoverable deleted items.',{exact:true}).waitFor();
 await page.keyboard.press('Escape');await trash.waitFor({state:'hidden'});
 await page.locator('#activitytab').click();await page.locator('.dtab[data-d="'+doc+'"]').click();
 await page.getByRole('button',{name:'Recover draft',exact:true}).click();await page.locator('#docsrc:not([readonly])').waitFor();assert.equal(await page.locator('#docsrc').inputValue(),'Local words that must survive deletion');
 await page.locator('#docsave').click();await page.locator('#doc .blk').filter({hasText:'Local words that must survive deletion'}).waitFor({state:'attached'});
 // Restoration advances versions, so a stale draft cannot remove restored paragraphs.
 await page.getByRole('dialog',{name:/paragraphs were left as they are/}).getByRole('button',{name:'Read it again',exact:true}).click();
 if(await page.locator('#docsrc').isVisible())await page.locator('#docmode').click();
 // Recovery in the same session must remove the exact draft stored at deletion.
 await page.locator('#docmode').click();await page.locator('#docsrc').fill('A second recoverable draft');
 await page.getByText('Draft on this device · Save to publish',{exact:true}).waitFor();
 await page.evaluate(async id=>{const src=document.querySelector('script[type=module]').src;const {send}=await import(new URL('net.js',src));await send('document.delete',{document:id});},doc);
 await page.getByText('Unsaved draft from deleted document #'+doc,{exact:true}).waitFor();
 const deleted=(await request('GET','/trash?proposition='+fixture.proposition)).items.find(item=>item.entity==='document'&&item.entity_id===doc);
 await request('POST','/trash/'+deleted.id+'/restore',{});
 await page.locator('.dtab[data-d="'+doc+'"]').click();await page.getByRole('button',{name:'Discard draft',exact:true}).click();
 await page.getByRole('dialog',{name:'Discard this unsaved draft?',exact:true}).getByRole('button',{name:'Discard draft',exact:true}).click();
 await page.getByRole('button',{name:'Recover draft',exact:true}).waitFor({state:'hidden'});
 await page.reload();assert.equal(await page.getByRole('button',{name:'Recover draft',exact:true}).count(),0);
 // Recovering browser storage with no queued uploads clears the error banner.
 await page.evaluate(()=>{const original=IDBObjectStore.prototype.getAll;window.failUploads=true;IDBObjectStore.prototype.getAll=function(...args){if(this.name==='uploads'&&window.failUploads)throw new Error('Temporary storage failure');return original.apply(this,args);};});
 await page.locator('.tabs [data-tab=files]').click();await page.getByRole('button',{name:'Retry upload recovery',exact:true}).waitFor();
 await page.evaluate(()=>window.failUploads=false);await page.getByRole('button',{name:'Retry upload recovery',exact:true}).click();await page.getByRole('button',{name:'Retry upload recovery',exact:true}).waitFor({state:'hidden'});
 await page.locator('.tabs [data-tab=board]').click();
 // Confirmation dialogs expose their purpose to accessibility APIs.
 await page.getByLabel('Document tools',{exact:true}).click();await page.getByRole('button',{name:'Delete this document',exact:true}).click();
 await page.getByRole('dialog',{name:/^Delete /}).waitFor();await page.keyboard.press('Escape');
 assert.deepEqual(errors,[]);
 console.log('PASS touch emulation, named dialogs, uncertain pin/review retries, deleted draft recovery, trash restore, responsive layouts');
}finally{await browser.close();}
