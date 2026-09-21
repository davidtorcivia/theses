import assert from 'node:assert/strict';
import { parseTarget, targetURL } from './static/app/anchors.js';
for (const kind of ['card','file','link','document','block','comment']) {
 assert.deepEqual(parseTarget('#'+kind+'-42'), {kind,id:42});
 assert.equal(targetURL(7,kind,42), '/p/7#'+kind+'-42');
}
for (const hash of ['#card-0','#card--1','#card-9007199254740993','#card-1junk','#other-42']) assert.equal(parseTarget(hash),null);
