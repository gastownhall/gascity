import type { ClientErrorReport } from 'gas-city-dashboard-shared';
import { errorMessage } from 'gas-city-dashboard-shared';

export type ClientErrorReportResult = { status: 'reported' } | { status: 'failed'; error: string };

export async function reportClientError(
  event: ClientErrorReport,
): Promise<ClientErrorReportResult> {
  // Same-origin custom-header CSRF: the /api/client-errors mutation guard only
  // requires a non-empty X-GC-Request header (a cross-site request cannot set a
  // custom header without a CORS preflight), so telemetry carries it directly —
  // no cookie double-submit. keepalive lets the report survive an unload.
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'Content-Type': 'application/json',
    'X-GC-Request': 'dashboard',
  };

  try {
    const res = await fetch('/api/client-errors', {
      method: 'POST',
      headers,
      credentials: 'same-origin',
      keepalive: true,
      body: JSON.stringify(event),
    });
    if (!res.ok) {
      return { status: 'failed', error: `client error report failed with ${res.status}` };
    }
    return { status: 'reported' };
  } catch (err) {
    return { status: 'failed', error: errorMessage(err) };
  }
}
