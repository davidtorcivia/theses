import { el, say } from './dom.js';
import { state, emit } from './state.js';
import { cleanFilters, defaultFilters } from './filters.js';
import { boardPreferences, keepBoardPreferences } from './offline.js';

let scope = '', controls, fields, choices = '';
let changed = 0, wanted = null, writing = false, timer = 0;
export function saveFilters() {
  changed++;
  wanted = { id: scope, value: cleanFilters({ ...state.boardOptions, status: state.boardFilter }) };
  clearTimeout(timer);
  timer = setTimeout(flushFilters, 200);
}
async function flushFilters() {
  clearTimeout(timer); timer = 0;
  if (writing) return;
  writing = true;
  try {
    while (wanted) {
      const row = wanted; wanted = null;
      if (await keepBoardPreferences(row.id, row.value) === null) say('Filters work here, but could not be saved on this device.');
    }
  } finally { writing = false; }
}
window.addEventListener('pagehide', flushFilters);
document.addEventListener('visibilitychange', () => { if (document.hidden) flushFilters(); });

export function boardControls() {
  const id = `${state.me}:${state.open}`;
  if (scope !== id) {
    scope = id; choices = ''; changed++;
    const revision = changed;
    state.boardOptions = defaultFilters();
    boardPreferences(id).then((row) => {
      if (scope !== id || changed !== revision || !row) return;
      state.boardOptions = cleanFilters(row.value);
      state.boardFilter = state.boardOptions.status;
      emit();
    });
    fields = {};
    controls = el('div', { class: 'board-controls' });
    for (const [name, label] of [['query','Search cards'],['assignee','Assignee'],['due','Due date'],['proposition','Linked proposition'],['view','Board view']]) {
      const input = el(name === 'query' ? 'input' : 'select', { id: 'filter-' + name, 'aria-label': label, type: name === 'query' ? 'search' : null });
      fields[name] = input;
      input.addEventListener(name === 'query' ? 'input' : 'change', () => {
        state.boardOptions[name] = input.value;
        saveFilters(); emit();
      });
      controls.append(el('label', {}, el('span', { class: 'mono', text: label }), input));
    }
    controls.append(el('button', { type: 'button', class: 'lnk', text: 'Clear filters', onclick: () => {
      state.boardOptions = defaultFilters(); state.boardFilter = 'all'; saveFilters(); emit();
    } }));
  }
  const key = JSON.stringify([state.props, [...state.users.values()]]);
  if (choices !== key) {
    choices = key;
    const options = {
      assignee: [['','Everyone'], ...[...state.users.values()].map((u) => [u.id,u.name])],
      proposition: [['','Any proposition'], ...state.props.filter((p) => p.kind !== 'show').map((p) => [p.id,p.title])],
      due: [['','Any date'],['overdue','Overdue'],['today','Today'],['upcoming','Upcoming'],['none','No date']],
      view: [['board','Board'],['list','List']],
    };
    for (const [name, rows] of Object.entries(options)) fields[name].replaceChildren(...rows.map(([value,text]) => el('option', { value, text })));
  }
  for (const [name, field] of Object.entries(fields)) {
    const value = state.boardOptions[name];
    if (field.tagName === 'SELECT' && value && ![...field.options].some((o) => o.value === value)) field.append(el('option', { value, text: 'Unavailable selection' }));
    if (field.value !== value) field.value = value;
  }
  return controls;
}
