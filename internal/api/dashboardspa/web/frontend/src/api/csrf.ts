const COOKIE_NAME = 'gascity_admin_csrf';

export type CsrfTokenResult = { status: 'available'; token: string } | { status: 'missing' };

export function readCsrfToken(): CsrfTokenResult {
  const match = document.cookie
    .split(';')
    .map((c) => c.trim())
    .find((c) => c.startsWith(COOKIE_NAME + '='));
  if (match === undefined) return { status: 'missing' };
  const token = decodeURIComponent(match.slice(COOKIE_NAME.length + 1));
  if (token.length === 0) return { status: 'missing' };
  return { status: 'available', token };
}
