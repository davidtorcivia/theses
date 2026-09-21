import assert from 'node:assert/strict';
import { monthDate, moveMonth, usHolidays } from './static/app/calendar.js';
assert.equal(moveMonth('2026-12',1),'2027-01');
assert.equal(moveMonth('2026-01',-1),'2025-12');
assert.equal(moveMonth('2000-01',-1),'2000-01');
assert.equal(moveMonth('2100-12',1),'2100-12');
for(const value of ['', '2026-00','2026-13','1999-12','2101-01','26-01'])assert.equal(monthDate(value),null);
assert.equal(monthDate('2028-02').getUTCMonth(),1);
const holidays=usHolidays(2026);
for(const [date,name] of [['2026-01-19','Martin Luther King Jr. Day'],['2026-02-16','Washington’s Birthday'],['2026-05-25','Memorial Day'],['2026-06-19','Juneteenth'],['2026-07-03','Independence Day (observed)'],['2026-07-04','Independence Day'],['2026-09-07','Labor Day'],['2026-10-12','Columbus Day'],['2026-11-11','Veterans Day'],['2026-11-26','Thanksgiving'],['2026-12-25','Christmas Day'],['2026-04-05','Easter Sunday'],['2026-05-10','Mother’s Day'],['2026-06-21','Father’s Day'],['2026-10-31','Halloween'],['2026-11-03','Election Day'],['2026-11-27','Black Friday']])assert.ok(holidays.get(date)?.includes(name),date+' '+name);
assert.ok(usHolidays(2027).get('2027-12-31').includes('New Year’s Day (observed)'));
assert.ok(usHolidays(2022).get('2022-12-26').includes('Christmas Day (observed)'));
assert.equal(usHolidays(2020).has('2020-06-19'),false);
for(const [year,date] of [[2027,'03-28'],[2028,'04-16'],[2038,'04-25']])assert.ok(usHolidays(year).get(year+'-'+date).includes('Easter Sunday'));
for(const year of [2026,2027,2028])assert.ok([...usHolidays(year).keys()].every(date=>date.startsWith(year+'-')));

assert.ok(usHolidays(2024).get('2024-11-29').includes('Black Friday'));
assert.equal([...usHolidays(2027).values()].flat().includes('Election Day'),false);
assert.ok(usHolidays(2022).get('2022-11-08').includes('Election Day'));
