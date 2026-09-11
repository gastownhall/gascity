// Thin frontend-side logging seam. Mirrors backend/src/logging.ts so
// frontend code can emit operator-visible warnings (e.g. PR-C's
// DEFAULT_VIEW shadowing per premortem #5) through a single named entry
// point rather than scattering `console.warn` calls past the global
// `no-console` ESLint rule.
//
// Today this wraps the browser console directly. A future telemetry hook
// (POST to /api/client-errors, sentry-style breadcrumb, etc.) lands here
// so individual call sites do not need to change.

export const LOG_COMPONENT = {
  views: 'views',
} as const;

export type LogComponent = (typeof LOG_COMPONENT)[keyof typeof LOG_COMPONENT];

export function logWarn(component: LogComponent, message: string): void {
  // eslint-disable-next-line no-console -- single-point seam (mirrors backend/logging.ts).
  console.warn(`[${component}] ${message}`);
}

export function logInfo(component: LogComponent, message: string): void {
  // eslint-disable-next-line no-console -- single-point seam (mirrors backend/logging.ts).
  console.info(`[${component}] ${message}`);
}
