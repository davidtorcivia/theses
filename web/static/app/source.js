// What a replayed whole-document save does to its editing session. The
// textarea and its text are deliberately untouched: the missing first answer
// may have contained conflicts, so only the base can safely move forward.
export const replayNotice = 'That save had already gone through. Your markdown is still here. Saving again merges it into the document as it now reads.';

export function retainReplay(src, doc) {
  src.base = (doc.blocks || []).map((block) => ({ id: block.id, version: block.version }));
  src.notice = replayNotice;
}
