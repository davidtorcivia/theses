import assert from 'node:assert/strict';
import fs from 'node:fs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const fixture=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const browser=await chromium.launch({headless:true,executablePath:process.env.BROWSER_EXECUTABLE||undefined});
try{
 const context=await browser.newContext({viewport:{width:1440,height:1000}});await context.addCookies(fixture.cookies);
 const page=await context.newPage();await page.addInitScript(()=>{window.clickTimes=[];document.addEventListener('click',()=>{const start=performance.now();requestAnimationFrame(()=>requestAnimationFrame(()=>window.clickTimes.push(performance.now()-start)));},true);});
 const results={};
 for(const [name,path] of [['show','/show'],['large','/p/'+fixture.large]]){
  await page.goto(fixture.url+path);await page.locator('#board').waitFor();
  if(name==='large')await page.waitForFunction(()=>document.querySelectorAll('#board .card').length===500);
  for(let i=0;i<15;i++){await page.locator('#activitytab').click();await page.locator('#drawer').waitFor({state:'visible'});await page.locator('#activitytab').click();await page.locator('#drawer').waitFor({state:'hidden'});}
  const myWork=page.locator('.connected summary').filter({hasText:'My work'});for(let i=0;i<5;i++){await myWork.click();await page.waitForTimeout(50);await myWork.click();}
  if(name==='show'){const calendar=page.locator('.production-calendar summary').first();for(let i=0;i<5;i++){await calendar.click();await page.waitForTimeout(50);await calendar.click();}}
  await page.waitForTimeout(100);const samples=await page.evaluate(()=>window.clickTimes.toSorted((a,b)=>a-b));results[name]={samples:samples.length,p95_ms:samples[Math.ceil(samples.length*.95)-1],max_ms:samples.at(-1)};
 }
 console.log('Click-to-next-painted-frame lab sample: '+JSON.stringify(results));
 if(process.env.THESES_ENFORCE_CLICK_BUDGET==='1')for(const result of Object.values(results))assert.ok(result.p95_ms<150,JSON.stringify(result));
}finally{await browser.close();}
