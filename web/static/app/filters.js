import { referenceIDs } from './references.js';

export function calendarDay(timezone, date = new Date()) {
  if (Number.isNaN(date.getTime())) return '';
  try {
    const parts = new Intl.DateTimeFormat('en-CA', { timeZone: timezone || undefined, year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(date);
    const part = (kind) => parts.find((p) => p.type === kind).value;
    return `${part('year')}-${part('month')}-${part('day')}`;
  } catch { return calendarDay('', date); }
}

export const defaultFilters = () => ({ assignee: '', due: '', proposition: '', query: '', view: 'board', status: 'all' });
export function cleanFilters(value) {
  const v = value || {};
  return {
    assignee: /^\d+$/.test(String(v.assignee)) ? String(v.assignee) : '',
    proposition: /^\d+$/.test(String(v.proposition)) ? String(v.proposition) : '',
    due: ['overdue', 'today', 'upcoming', 'none'].includes(v.due) ? v.due : '',
    status: ['all', 'mine', 'open'].includes(v.status) ? v.status : 'all',
    view: v.view === 'list' ? 'list' : 'board',
    query: typeof v.query === 'string' ? v.query.slice(0, 200) : '',
  };
}

export function matchesCard(card, filter, me, today) {
  const assigned = card.assignees || [];
  if (filter.status === 'mine' && !assigned.includes(me)) return false;
  if (filter.status === 'open' && card.done_at) return false;
  if (filter.assignee && !assigned.includes(Number(filter.assignee))) return false;
  if (filter.proposition && !referenceIDs(card.title + '\n' + (card.description_md || '')).includes(Number(filter.proposition))) return false;
  if (filter.query && !(card.title + '\n' + (card.description_md || '')).toLocaleLowerCase().includes(filter.query.toLocaleLowerCase())) return false;
  const due = card.due_date || '';
  if (filter.due === 'none') return !due;
  if (filter.due && (card.done_at || !due)) return false;
  if (filter.due === 'overdue') return due < today;
  if (filter.due === 'today') return due === today;
  if (filter.due === 'upcoming') return due > today;
  return true;
}
