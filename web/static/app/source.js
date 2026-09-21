// What a replayed whole-document save does to its editing session. The
// textarea and its text are deliberately untouched: the missing first answer
// may have contained conflicts, so only the base can safely move forward.
export const replayNotice = 'That save had already gone through. Your markdown is still here. Saving again merges it into the document as it now reads.';

export function retainReplay(src, doc) {
  src.base = (doc.blocks || []).map((block) => ({ id: block.id, version: block.version }));
  src.notice = replayNotice;
}

export function sourceAttempt(src, key) {
  if (!src.pending) src.pending = { key, text: src.text, base: structuredClone(src.base) };
  return src.pending;
}

export function draftMatches(row, account, proposition, doc) {
  return row.account === account && row.proposition === proposition
    && row.document === doc.id && row.created === doc.created_at;
}

export function draftRecord(src, account, proposition, doc) {
  return structuredClone({ id: src.draftID, account, proposition, document: doc.id,
    created: doc.created_at, base: src.base, text: src.text, clean: src.clean,
    pending: src.pending || null, needsAttention: Boolean(src.needsAttention), notice: src.notice, at: Date.now() });
}

export const sourceDirty = (src) => Boolean(src && (src.text !== src.clean || src.pending || src.needsAttention || src.storageFailed || src.saving));
