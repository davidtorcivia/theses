export const propositionToken = (id) => `@[p:${id}]`;
export const propositionURL = (p) => p.kind === 'show' ? '/show' : '/p/' + p.id;
export const propositionLabel = (p) => p.kind === 'show' ? p.title : `${String(p.number).padStart(2, '0')} ${p.title}`;

export function referenceIDs(text) {
  return [...new Set([...text.matchAll(/`+[^\n]*?`+|(?<![\p{L}\p{N}_])@\[p:([1-9]\d*)\]/gu)].map((m) => Number(m[1])).filter(Number.isSafeInteger))];
}

export function referenceKey(text, lookup) {
  return JSON.stringify(referenceIDs(text).map((id) => {
    const p = lookup(id);
    return p ? [id, p.title, p.number, p.status, p.archived_at] : [id];
  }));
}

export function propositionMatches(propositions, query) {
  const q = query.toLocaleLowerCase();
  return propositions.filter((p) => p.kind !== 'show' && !p.archived_at &&
    (propositionLabel(p).toLocaleLowerCase().includes(q))).slice(0, 12);
}
