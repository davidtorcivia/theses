import assert from 'node:assert/strict';
import fs from 'node:fs';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fixture = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const browser = await chromium.launch({ headless: true, executablePath: process.env.BROWSER_EXECUTABLE || undefined });
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  await context.addCookies(fixture.cookies);
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', err => errors.push(err.message));
  page.on('dialog', dialog => dialog.accept());
  await page.addInitScript(() => {
    window.longTasks = [];
    new PerformanceObserver(list => window.longTasks.push(...list.getEntries().map(e => e.duration))).observe({ type: 'longtask', buffered: true });
  });
  const visit = path => page.goto(fixture.url + path);
  // waitForFunction treats a returned Promise as truthy before it resolves.
  const waitAsync = async (predicate, argument) => {
    const deadline = Date.now() + 15000;
    while (!(await page.evaluate(predicate, argument))) {
      assert.ok(Date.now() < deadline, 'Asynchronous browser condition timed out');
      await page.waitForTimeout(100);
    }
  };
  const toggleActivity = async () => {
    const button = page.locator('#activitytab');
    await button.click();
    await page.waitForFunction(() => document.querySelector('#activitytab')?.getAttribute('aria-expanded') === 'true' && !document.querySelector('#drawer').hidden);
    await button.click();
    await page.locator('#drawer').waitFor({ state: 'hidden' });
    assert.equal(await button.getAttribute('aria-expanded'), 'false');
    await button.focus(); await page.keyboard.press('Enter');
    await page.waitForFunction(() => document.querySelector('#activitytab')?.getAttribute('aria-expanded') === 'true');
    await page.keyboard.press('Space');
    await page.locator('#drawer').waitFor({ state: 'hidden' });
    assert.equal(await button.evaluate(node => node === document.activeElement), true);
  };
  await visit('/show');
  await page.locator('#board').waitFor();
  await toggleActivity();
  for (const selector of ['#newprop', '.ws.showpin']) {
    const control = page.locator(selector);
    await control.hover();
    assert.match(await control.evaluate(node => getComputedStyle(node.querySelector('.t') || node).textDecorationLine), /underline/);
  }
  assert.equal(await page.locator('.tabs [data-tab=notes]').count(), 0);
  await page.locator('#docmode').click();
  await page.locator('#docsrc').fill('# Research\n\nDurable draft');
  await page.getByText('Draft on this device · Save to publish', { exact: true }).waitFor();
  await page.reload();
  await page.getByRole('button', { name: 'Recover draft', exact: true }).click();
  await page.locator('#docsrc:not([readonly])').waitFor();
  assert.match(await page.locator('#docsrc').inputValue(), /Durable draft/);
  let release, seen;
  const intercepted = new Promise(resolve => seen = resolve);
  await page.route('**/app/documents/*/source', async route => {
    seen(); await new Promise(resolve => release = resolve); await route.continue();
  });
  await page.locator('#docsave').click(); await intercepted;
  await page.locator('#docsrc').fill('# Research\n\nDurable draft\n\nTyped during save');
  release();
  await page.getByText('Draft on this device · Save to publish', { exact: true }).waitFor();
  await page.unroute('**/app/documents/*/source');
  await page.locator('#docsave').click();
  await page.locator('#doc .blk').filter({ hasText: 'Typed during save' }).waitFor();
  await page.locator('.outline summary').click();
  await page.locator('.outline a').first().click();
  await page.waitForFunction(() => document.activeElement?.classList.contains('blk'));
  await page.locator('.board-add').first().getByRole('button', { name: '+ Card', exact: true }).click();
  await page.locator('.newcard textarea').fill(`Plan @[p:${fixture.proposition}]`);
  await page.locator('.newcard textarea').press('Enter');
  const linked = page.locator('#board .card').filter({ hasText: 'Tidal Power' });
  await linked.waitFor(); await linked.click();
  const cardURL = page.url(); assert.match(cardURL, /#card-\d+$/);
  await page.reload(); await page.locator('#drawer h2').waitFor();
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await page.goBack(); await page.locator('#drawer h2').waitFor();
  await page.goForward(); await page.locator('#drawer').waitFor({ state: 'hidden' });
  await linked.click(); await page.locator('#activitytab').click();
  await page.waitForFunction(() => location.hash === '#activity' && document.querySelector('#activitytab')?.getAttribute('aria-expanded') === 'true');
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await page.locator('#drawer').waitFor({ state: 'hidden' });
  await page.waitForFunction(() => document.activeElement?.id === 'activitytab');
  await page.reload(); await page.locator('#board').waitFor();
  await page.locator('#drawer').waitFor({ state: 'hidden' });
  await page.locator('#filter-view').selectOption('list');
  await page.locator('#filter-query').fill('Plan');
  await page.waitForTimeout(300); await page.reload();
  await page.waitForFunction(() => document.querySelector('#filter-query')?.value === 'Plan');
  assert.equal(await page.locator('#filter-view').inputValue(), 'list');
  await page.getByRole('button', { name: 'Clear filters', exact: true }).click();
  await linked.click();
  const move = page.getByRole('combobox', { name: 'Move to column', exact: true });
  await move.waitFor();
  const choices = await move.locator('option').evaluateAll(rows => rows.map(row => row.value));
  const previous = await move.inputValue(); const next = choices.find(id => id !== previous);
  const movedID = Number(new URL(page.url()).hash.slice(6));
  await move.selectOption(next);
  await waitAsync(async ({id,next}) => { const html=await (await fetch(location.pathname)).text();const document=new DOMParser().parseFromString(html,'text/html');const payload=JSON.parse(document.querySelector('#payload').textContent);return payload.board.cards.some(c=>c.id===id&&String(c.column_id)===next); }, {id:movedID,next});
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await page.getByText('Production calendar', { exact: true }).click(); await page.getByRole('table').waitFor();
  await page.getByLabel('Production month',{exact:true}).fill('2026-11');
  await page.getByRole('heading',{name:'November 2026',exact:true}).waitFor();
  assert.equal(await page.locator('.production-calendar thead th').count(),7);
  await page.locator('.calendar-scroll').getByText('Thanksgiving',{exact:true}).waitFor();
  await page.getByRole('button',{name:'Next month',exact:true}).click();
  await page.getByRole('heading',{name:'December 2026',exact:true}).waitFor();
  await page.getByRole('button',{name:'Previous month',exact:true}).click();
  await page.getByLabel('U.S. holidays & observances',{exact:true}).uncheck();
  assert.equal(await page.locator('.calendar-holiday').count(),0);
  await page.getByLabel('U.S. holidays & observances',{exact:true}).check();
  await page.getByLabel('Production month',{exact:true}).fill('2028-02');
  assert.equal(await page.locator('.calendar-scroll td time').count(),29);
  await page.getByLabel('Production month',{exact:true}).fill('2026-09');

  await visit('/p/' + fixture.proposition + '#activity');
  await page.waitForFunction(() => document.querySelector('#activitytab')?.getAttribute('aria-expanded') === 'true');
  await page.locator('#activitytab').click();
  await page.locator('#drawer').waitFor({ state: 'hidden' });
  assert.notEqual(new URL(page.url()).hash, '#activity');
  await toggleActivity();
  await page.locator('.ws.showpin').hover();
  assert.match(await page.locator('.ws.showpin .t').evaluate(node => getComputedStyle(node).textDecorationLine), /underline/);
  await page.getByText('Production readiness', { exact: true }).click();
  await page.getByLabel('Next action',{exact:true}).fill('Check citations before recording');
  await page.route('**/app/production?*',route=>route.abort());
  await page.getByText('Production readiness',{exact:true}).click();
  await page.getByText('Production readiness',{exact:true}).click();
  await page.getByRole('button',{name:'Retry readiness',exact:true}).waitFor();
  assert.equal(await page.getByLabel('Next action',{exact:true}).inputValue(),'Check citations before recording');
  await page.unroute('**/app/production?*');
  await page.getByLabel('Production owner',{exact:true}).selectOption(String(fixture.owner));
  await page.getByLabel('Record by',{exact:true}).fill('2026-09-25');
  await page.getByRole('button',{name:'Save production plan',exact:true}).click();
  await page.locator('.production-plan').getByText('Saved',{exact:true}).waitFor();
  await page.getByRole('button', { name: 'Create production checklist', exact: true }).click();
  await page.locator('#drawer h2').filter({ hasText: 'Production checklist' }).waitFor();
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await page.getByRole('button', { name: 'Activity', exact: true }).click();
  const filtered = page.waitForResponse(response => response.url().includes('/app/activity?') && new URL(response.url()).searchParams.get('entity') === 'file');
  await page.getByRole('combobox', { name: 'Activity type', exact: true }).selectOption('file');
  const history = await (await filtered).json(); assert.ok((history.activity || history.rows || []).every(row => row.entity === 'file'));
  await page.getByRole('heading', { name: 'History', exact: true }).waitFor();
  await page.locator('#drawer').getByText('Nothing yet.',{exact:true}).waitFor();
  await page.getByRole('button', { name: 'Close', exact: true }).click();

  await page.getByRole('button',{name:'Recording view',exact:true}).click();
  await page.locator('.recording-pin summary').click();
  await page.getByLabel('Recording cues',{exact:true}).fill('Pause before the conclusion.');
  if(process.env.THESES_SCREENSHOT_DIR)await page.locator('.recording-view').screenshot({animations:'disabled',path:process.env.THESES_SCREENSHOT_DIR+'/recording-view.png'});
  await page.getByRole('button',{name:'Pin saved script',exact:true}).click();
  await page.locator('.recording-cues').filter({hasText:'Pause before the conclusion.'}).waitFor();
  await page.getByRole('button',{name:'Start timer',exact:true}).click();
  await page.getByRole('button',{name:'Pause timer',exact:true}).waitFor();
  await page.locator('dialog header').getByRole('button',{name:'Close',exact:true}).click();
  await page.getByRole('button',{name:'Review',exact:true}).click();
  await page.getByRole('button',{name:'Request review of current version',exact:true}).click();
  await page.getByRole('button',{name:'Approve version',exact:true}).click();
  await page.locator('.review-item strong').filter({hasText:'approved'}).waitFor();
  await page.getByRole('button',{name:'Request review of current version',exact:true}).click();
  await page.getByLabel('Review feedback',{exact:true}).nth(1).waitFor();
  if(process.env.THESES_SCREENSHOT_DIR)await page.locator('.workflow-dialog').screenshot({animations:'disabled',path:process.env.THESES_SCREENSHOT_DIR+'/review-dialog.png'});
  await page.getByLabel('Review feedback',{exact:true}).nth(0).fill('Keep this unfinished feedback.');
  await page.getByRole('button',{name:'Approve version',exact:true}).nth(1).click();
  await page.waitForFunction(()=>document.querySelector('[aria-label="Review feedback"]')?.value==='Keep this unfinished feedback.');
  await page.getByRole('button',{name:'Approve version',exact:true}).nth(0).click();
  await page.locator('.review-item').getByText('Keep this unfinished feedback.',{exact:true}).waitFor();
  await page.locator('dialog header').getByRole('button',{name:'Close',exact:true}).click();
  await page.getByText('Research & references',{exact:true}).click();
  const checkResearchLayout=async()=>{
    const layout=await page.locator('.research-toolbar').evaluate(node=>{
      const rects=[...node.children].map(child=>{const r=child.getBoundingClientRect();return {x:r.x,y:r.y,right:r.right,bottom:r.bottom,height:r.height};});
      return {display:getComputedStyle(node).display,rects,overflow:document.documentElement.scrollWidth>innerWidth};
    });
    assert.ok(['flex','grid'].includes(layout.display));assert.equal(layout.overflow,false);
    for(const [i,a] of layout.rects.entries()){
      assert.ok(a.height>=38);
      for(const b of layout.rects.slice(i+1))assert.ok(a.right+4<=b.x||b.right+4<=a.x||a.bottom+4<=b.y||b.bottom+4<=a.y,'Research actions need a visible gap');
    }
  };
  await page.locator('.research-toolbar').waitFor();await checkResearchLayout();

  await page.getByRole('button',{name:'Add evidence',exact:true}).click();
  await page.getByLabel('Reference title',{exact:true}).fill('Energy reference');
  if(process.env.THESES_SCREENSHOT_DIR)await page.locator('.workflow-dialog').screenshot({animations:'disabled',path:process.env.THESES_SCREENSHOT_DIR+'/evidence-dialog.png'});
  await page.getByLabel('Exact quotation',{exact:true}).fill('A checked source quotation.');
  await page.getByLabel('Page, section, or timestamp',{exact:true}).fill('p. 42');
  await page.getByLabel('Source checked and claim verified',{exact:true}).check();
  await page.getByRole('button',{name:'Save evidence',exact:true}).click();
  await page.locator('.evidence-item strong').filter({hasText:'Energy reference'}).waitFor();

  for(const width of [320,390,768,1440]){
    await page.setViewportSize({width,height:1000});await checkResearchLayout();
    if(process.env.THESES_SCREENSHOT_DIR)await page.locator('.research').screenshot({animations:'disabled',style:'#top{visibility:hidden}',path:process.env.THESES_SCREENSHOT_DIR+'/research-'+width+'.png'});
  }
  const evidenceId=await page.locator('.evidence-item').first().getAttribute('id');
  await visit('/p/'+fixture.proposition+'#'+evidenceId);
  await page.waitForFunction(id=>document.activeElement?.id===id,evidenceId);
  const ris=await context.request.get(fixture.url+'/app/evidence/export?proposition='+fixture.proposition+'&format=ris');
  assert.match(await ris.text(),/TI  - Energy reference/);
  await visit('/p/' + fixture.proposition + '#files');
  await toggleActivity();
  // The fake S3 server is loopback-only; CORS is supplied at this test boundary.
  let failed = false;
  await context.route('**/browser-fixture/**', async route => {
    const request = route.request();
    if (request.method() === 'OPTIONS') { await route.fulfill({ status: 204, headers: { 'Access-Control-Allow-Origin': '*', 'Access-Control-Allow-Methods': 'GET,HEAD,PUT', 'Access-Control-Allow-Headers': '*' } }); return; }
    if (request.method() === 'PUT' && !failed) { failed = true; await route.abort('failed'); return; }
    const response = await route.fetch();
    await route.fulfill({ response, headers: { ...response.headers(), 'Access-Control-Allow-Origin': '*', 'Access-Control-Expose-Headers': 'ETag' } });
  });
  await page.locator('#drop input[type=file]').setInputFiles({ name: 'browser-upload.txt', mimeType: 'text/plain', buffer: Buffer.from('Recovered upload') });
  await page.getByRole('button', { name: /retry/i }).first().waitFor();
  await page.getByRole('button', { name: /retry/i }).first().click();
  await page.locator('#flist .row').filter({ hasText: 'browser-upload.txt' }).click();
  await page.getByRole('button', { name: 'Download', exact: true }).waitFor();
  await page.getByRole('button', { name: 'Edit notes and tags', exact: true }).click();
  await page.getByRole('textbox', { name: 'File notes', exact: true }).fill('Searchable upload notes');
  await page.getByRole('textbox', { name: 'Tags', exact: true }).fill('Research, source');
  await page.getByRole('button', { name: 'Save notes', exact: true }).click();
  await page.getByText('Searchable upload notes', { exact: true }).waitFor();
  const fileURL = page.url(); await page.reload(); await page.getByText('Searchable upload notes', { exact: true }).waitFor();
  await visit('/p/'+fixture.proposition+'#file-'+fixture.recording);
  await page.locator('.transcript summary').click();
  await page.getByLabel('Import transcript',{exact:true}).setInputFiles({name:'studio.vtt',mimeType:'text/vtt',buffer:Buffer.from('WEBVTT\n\n00:00:01.000 --> 00:00:03.000\n<v Ada>The studio transcript phrase.</v>\n')});
  await page.locator('.transcript-segment').filter({hasText:'The studio transcript phrase.'}).waitFor();
  await page.getByLabel('Search transcript',{exact:true}).fill('studio');
  await page.getByRole('button',{name:'Edit passage / speaker',exact:true}).click();
  await page.getByLabel('Speaker label',{exact:true}).fill('Host Ada');
  await page.getByRole('button',{name:'Save passage',exact:true}).click();
  await page.locator('.transcript-segment strong').filter({hasText:'Host Ada'}).waitFor();
  if(process.env.THESES_SCREENSHOT_DIR)await page.locator('.transcript').screenshot({animations:'disabled',path:process.env.THESES_SCREENSHOT_DIR+'/transcript-controls.png'});
  await page.getByRole('button',{name:'Add as comment',exact:true}).click();
  await page.getByRole('button',{name:'Resolve',exact:true}).click();
  await page.getByRole('button',{name:'Reopen',exact:true}).waitFor();
  await page.getByRole('button',{name:'Review this audio version',exact:true}).click();
  await page.getByRole('button',{name:'Request review of current version',exact:true}).click();
  await page.getByRole('button',{name:'Approve version',exact:true}).click();
  await page.locator('.review-item strong').filter({hasText:'approved'}).waitFor();
  await page.locator('dialog header').getByRole('button',{name:'Close',exact:true}).click();
  const transcriptExport=await context.request.get(fixture.url+'/app/files/'+fixture.recording+'/transcript/export?format=vtt');
  assert.match(await transcriptExport.text(),/<v Host Ada>/);
  const anonymous = await browser.newContext();
  const denied = await anonymous.newPage(); await denied.goto(fileURL);
  assert.match(denied.url(), /\/login/); await anonymous.close();
  await page.locator('#activitytab').click();
  await page.waitForFunction(() => location.hash === '#activity' && document.querySelector('#activitytab')?.getAttribute('aria-expanded') === 'true');
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await visit('/show'); await page.locator('#board').waitFor();
  await page.evaluate(() => navigator.serviceWorker.ready);
  await page.waitForFunction(() => navigator.serviceWorker.controller);
  await page.evaluate(() => window.previousController = navigator.serviceWorker.controller);
  await page.evaluate(async () => { const old = await caches.open('theses-stale-fixture'); await old.put('/stale', new Response('old')); await fetch('/__smoke/upgrade'); const reg = await navigator.serviceWorker.getRegistration(); await reg.update(); });
  await waitAsync(async () => !(await caches.keys()).includes('theses-stale-fixture') && navigator.serviceWorker.controller !== window.previousController && navigator.serviceWorker.controller?.state === 'activated');
  await waitAsync(() => new Promise(resolve => { const request=indexedDB.open('theses-offline'); request.onsuccess=()=>{const db=request.result;const read=db.transaction('snapshot').objectStore('snapshot').getAll();read.onsuccess=()=>{const rows=read.result;db.close();resolve(rows.some(row=>row.payload.propositions.some(p=>p.kind==='show' && p.id===row.proposition)));};}; }));
  await context.setOffline(true); await visit('/show'); await page.locator('#board').waitFor();
  await page.locator('#docmode').click(); await page.locator('#docsrc').fill('Offline source draft');
  await page.getByText('Offline · draft on this device', { exact: true }).waitFor();
  await context.setOffline(false);
  await page.waitForFunction(() => JSON.parse(document.querySelector('#payload')?.textContent || 'null')?.me);
  await page.getByRole('button', { name: 'Recover draft', exact: true }).click(); await page.locator('#docsrc:not([readonly])').waitFor();
  assert.equal(await page.locator('#docsrc').inputValue(), 'Offline source draft'); await page.locator('#docsave').click();
  await page.locator('#doc .blk').filter({ hasText: 'Offline source draft' }).waitFor();
  await visit('/p/' + fixture.large);
  await page.waitForFunction(() => document.querySelectorAll('#board .card').length === 500);
  const filterStart = performance.now();
  await page.locator('#filter-query').fill('Research task 499');
  await page.waitForFunction(() => document.querySelectorAll('#board .card').length === 1);
  const filter_ms = performance.now() - filterStart;
  await page.getByRole('button', { name: 'Clear filters', exact: true }).click();
  await page.waitForFunction(() => document.querySelectorAll('#board .card').length === 500);
  if(process.env.THESES_SCREENSHOT_DIR){await page.screenshot({path:process.env.THESES_SCREENSHOT_DIR+'/large-desktop.png',fullPage:true});}
  await page.setViewportSize({ width: 390, height: 844 });
  const overflow=await page.evaluate(()=>[...document.querySelectorAll('body *')].filter(n=>n.getBoundingClientRect().right>innerWidth+1).slice(0,15).map(n=>({tag:n.tagName,class:n.className,right:n.getBoundingClientRect().right})));
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false,JSON.stringify(overflow));
  await page.getByRole('button', { name: 'Activity', exact: true }).click(); await page.keyboard.press('Escape');
  const timings = await page.evaluate(() => ({ navigation_ms: performance.getEntriesByType('navigation')[0]?.duration, long_tasks: window.longTasks.length, longest_task_ms: Math.max(0, ...window.longTasks), heap_bytes: performance.memory?.usedJSHeapSize }));
  if(process.env.THESES_SCREENSHOT_DIR){await visit('/show');await page.locator('.production-calendar summary').first().click();await page.locator('.calendar-agenda').waitFor();await page.getByText('My work',{exact:true}).click();await page.screenshot({path:process.env.THESES_SCREENSHOT_DIR+'/show-mobile.png',fullPage:true});await page.setViewportSize({width:1440,height:1000});await page.screenshot({path:process.env.THESES_SCREENSHOT_DIR+'/show-desktop.png',fullPage:true});}

  await visit('/show');await page.locator('.production-calendar summary').first().click();
  await page.getByLabel('Production month',{exact:true}).fill('2026-11');
  for(const width of [320,390,768,1440]){
    await page.setViewportSize({width,height:1000});
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'Calendar overflow at '+width);
    await page.getByRole('table').waitFor();
    if(process.env.THESES_SCREENSHOT_DIR)await page.locator('.production-calendar').screenshot({animations:'disabled',style:'#top{visibility:hidden}',path:process.env.THESES_SCREENSHOT_DIR+'/calendar-'+width+'.png'});
  }
  await page.getByRole('link',{name:'Calendar sync',exact:true}).click();
  await page.getByRole('button',{name:'Create subscription link',exact:true}).click();
  const calendarURL=await page.locator('#calendar-url').inputValue();
  await context.grantPermissions(['clipboard-read','clipboard-write']);
  await page.getByRole('button',{name:'Copy subscription link',exact:true}).click();
  assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),calendarURL);
  const calendarPath=new URL(calendarURL).pathname;
  const subscriber=await browser.newContext();
  const feed=await subscriber.request.get(fixture.url+calendarPath);
  assert.equal(feed.status(),200);assert.match(await feed.text(),/BEGIN:VCALENDAR/);
  for(const width of [320,1440]){
    await page.setViewportSize({width,height:1000});
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'Calendar settings overflow at '+width);
    for(const button of await page.locator('#calendar button').all())assert.ok((await button.boundingBox()).height>=40,'Calendar sync touch target');
    if(process.env.THESES_SCREENSHOT_DIR)await page.locator('#calendar').screenshot({animations:'disabled',style:'#top{visibility:hidden}',path:process.env.THESES_SCREENSHOT_DIR+'/calendar-sync-'+width+'.png'});
  }
  await page.getByRole('button',{name:'Replace subscription link',exact:true}).click();
  const replacement=new URL(await page.locator('#calendar-url').inputValue()).pathname;
  assert.notEqual(replacement,calendarPath);
  assert.equal((await subscriber.request.get(fixture.url+calendarPath)).status(),404);
  assert.equal((await subscriber.request.get(fixture.url+replacement)).status(),200);
  await page.getByRole('button',{name:'Revoke subscription',exact:true}).click();
  assert.equal((await subscriber.request.get(fixture.url+replacement)).status(),404);
  await page.getByRole('button',{name:'Create subscription link',exact:true}).waitFor();
  await subscriber.close();
  assert.deepEqual(errors, []);
  console.log('PASS drafts, mentions, links, filters, move controls, outline, production, history, upload retry, notes, private access, offline recovery, worker cache replacement, mobile');
  console.log('Lab sample (not field INP): ' + JSON.stringify({ cards: 500, filter_ms, ...timings }));
} finally { await browser.close(); }
