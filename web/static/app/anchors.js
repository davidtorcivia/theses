import { el, say } from './dom.js';

export function parseTarget(hash) {
  const match = /^#(card|file|link|document|block|comment)-([1-9]\d*)$/.exec(hash);
  if (!match || !Number.isSafeInteger(Number(match[2]))) return null;
  return { kind: match[1], id: Number(match[2]) };
}

export function targetURL(proposition, kind, id) {
  return `/p/${proposition}#${kind}-${id}`;
}

export function rememberTarget(kind, id) {
  const hash = kind ? `#${kind}-${id}` : '#' + (id || 'board');
  if (location.hash !== hash) history.pushState(null, '', hash);
}

export function copyTarget(proposition, kind, id) {
  return el('button', { class: 'lnk', type: 'button', text: 'Copy link',
    onclick: async (e) => {
      e.stopPropagation();
      try {
        await navigator.clipboard.writeText(new URL(targetURL(proposition, kind, id), location.origin).href);
        say('Link copied.');
      } catch { say('Could not copy the link.'); }
    } });
}
