export function monthDate(value) {
  if (!/^\d{4}-(0[1-9]|1[0-2])$/.test(value)) return null;
  const [year, month] = value.split('-').map(Number);
  return year >= 2000 && year <= 2100 ? new Date(Date.UTC(year, month - 1, 1)) : null;
}

export function moveMonth(value, step) {
  const date = monthDate(value);
  if (!date) return value;
  date.setUTCMonth(date.getUTCMonth() + step);
  const next = date.toISOString().slice(0, 7);
  return monthDate(next) ? next : value;
}

export function usHolidays(year) {
  const days = new Map();
  const add = (date, name) => {
    if (date.getUTCFullYear() !== year) return;
    const key = date.toISOString().slice(0, 10);
    days.set(key, [...(days.get(key) || []), name]);
  };
  const fixed = (y, month, day, name) => {
    const date = new Date(Date.UTC(y, month - 1, day));
    add(date, name);
    const weekday = date.getUTCDay();
    if (weekday === 0 || weekday === 6) {
      date.setUTCDate(day + (weekday === 0 ? 1 : -1));
      add(date, name + ' (observed)');
    }
  };
  const weekday = (month, day, nth, name) => {
    const date = new Date(Date.UTC(year, month - 1, 1));
    if (nth === -1) {
      date.setUTCMonth(month, 0);
      date.setUTCDate(date.getUTCDate() - (date.getUTCDay() - day + 7) % 7);
    } else date.setUTCDate(1 + (day - date.getUTCDay() + 7) % 7 + (nth - 1) * 7);
    add(date, name);
    return date;
  };
  fixed(year, 1, 1, 'New Year’s Day');
  // A Saturday New Year is observed in the preceding calendar year.
  fixed(year + 1, 1, 1, 'New Year’s Day');
  weekday(1, 1, 3, 'Martin Luther King Jr. Day');
  weekday(2, 1, 3, 'Washington’s Birthday');
  weekday(5, 1, -1, 'Memorial Day');
  if (year >= 2021) fixed(year, 6, 19, 'Juneteenth');
  fixed(year, 7, 4, 'Independence Day');
  weekday(9, 1, 1, 'Labor Day');
  weekday(10, 1, 2, 'Columbus Day');
  fixed(year, 11, 11, 'Veterans Day');
  const thanksgiving = weekday(11, 4, 4, 'Thanksgiving');
  thanksgiving.setUTCDate(thanksgiving.getUTCDate()+1);
  add(thanksgiving,'Black Friday');
  if(year%2===0){
    const election=new Date(Date.UTC(year,10,2));
    election.setUTCDate(2+(2-election.getUTCDay()+7)%7);
    add(election,'Election Day');
  }
  fixed(year, 12, 25, 'Christmas Day');
  for(const [month,day,name] of [[2,14,'Valentine’s Day'],[3,17,'St. Patrick’s Day'],[10,31,'Halloween'],[12,24,'Christmas Eve'],[12,31,'New Year’s Eve']])add(new Date(Date.UTC(year,month-1,day)),name);
  weekday(5,0,2,'Mother’s Day');
  weekday(6,0,3,'Father’s Day');
  // Gregorian Easter, using the century-corrected computus.
  const a=year%19,b=Math.floor(year/100),c=year%100;
  const d=Math.floor(b/4),e=b%4,f=Math.floor((b+8)/25),g=Math.floor((b-f+1)/3);
  const h=(19*a+b-d-g+15)%30,i=Math.floor(c/4),k=c%4;
  const l=(32+2*e+2*i-h-k)%7,m=Math.floor((a+11*h+22*l)/451);
  const n=h+l-7*m+114;
  add(new Date(Date.UTC(year,Math.floor(n/31)-1,n%31+1)),'Easter Sunday');
  return days;
}
