export function isJSON(res) {
  return /json/i.test(res.headers?.get?.('Content-Type') || '');
}

// Only our own sign-in page proves session expiry; other HTML may come from a proxy.
export function sessionEnded(res, base) {
  if (isJSON(res)) return false;
  const answered = new URL(res.url || '', base);
  return answered.origin === new URL(base).origin && answered.pathname === '/login';
}
