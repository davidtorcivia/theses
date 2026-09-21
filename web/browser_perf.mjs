import assert from 'node:assert/strict';
import fs from 'node:fs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const fixture=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const browser=await chromium.launch({headless:true,executablePath:process.env.BROWSER_EXECUTABLE||undefined});
try{
 const context=await browser.newContext({viewport:{width:1440,height:1000}});await context.addCookies(fixture.cookies);
 const page=await context.newPage();await page.addInitScript(()=>{
  window.clickTimes=[];window.interactionTimes=[];
  document.addEventListener('click',()=>{const start=performance.now();requestAnimationFrame(()=>requestAnimationFrame(()=>window.clickTimes.push(performance.now()-start)));},true);
  new PerformanceObserver(list=>{for(const event of list.getEntries())if(event.interactionId)window.interactionTimes.push(event.duration);}).observe({type:'event',buffered:true,durationThreshold:16});
 });
 const results={};const cdp=await context.newCDPSession(page);
 for(const slowed of [false,true]){
  await cdp.send('Emulation.setCPUThrottlingRate',{rate:slowed?2:1});
  if(slowed)await page.route('**/app/**',async route=>{await new Promise(resolve=>setTimeout(resolve,300));await route.continue();});
  for(const [name,path] of [['show','/show'],['large','/p/'+fixture.large]]){
   await page.goto(fixture.url+path);await page.locator('#board').waitFor();
   if(name==='large')await page.waitForFunction(()=>document.querySelectorAll('#board .card').length===500&&document.querySelectorAll('#doc .blk').length>=200);
   await page.evaluate(()=>{window.clickTimes=[];window.interactionTimes=[];});
   for(let i=0;i<10;i++){await page.locator('#activitytab').click();await page.locator('#drawer').waitFor({state:'visible'});await page.locator('#activitytab').click();await page.locator('#drawer').waitFor({state:'hidden'});}
   const myWork=page.locator('.connected summary').filter({hasText:'My work'});for(let i=0;i<5;i++){await myWork.click();await page.waitForTimeout(50);await myWork.click();}
   if(name==='show'){const calendar=page.locator('.production-calendar summary').first();for(let i=0;i<5;i++){await calendar.click();await page.waitForTimeout(50);await calendar.click();}}
   else for(let i=0;i<5;i++){await page.locator('#docmode').click();await page.locator('#docsrc').waitFor({state:'visible'});await page.locator('#docmode').click();await page.locator('#doc').waitFor({state:'visible'});}
   await page.waitForTimeout(100);
   results[(slowed?'slow_':'')+name]=await page.evaluate(()=>{const samples=window.clickTimes.toSorted((a,b)=>a-b);const events=window.interactionTimes.toSorted((a,b)=>a-b);return {samples:samples.length,p95_ms:samples[Math.ceil(samples.length*.95)-1],max_ms:samples.at(-1),event_p95_ms:events[Math.ceil(events.length*.95)-1]||0};});
  }
 }
 console.log('Click-to-next-painted-frame lab sample (slow: 300ms request delay, 2x CPU; large: 500 cards, 200 long paragraphs): '+JSON.stringify(results));
 if(process.env.THESES_ENFORCE_CLICK_BUDGET==='1')for(const result of Object.values(results))assert.ok(result.p95_ms<150,JSON.stringify(result));
}finally{await browser.close();}
