import type { SupervisorBead } from '../supervisor/beadReads';

// Application-preview resolution for a Workbench Execution Attempt.
//
// The preview is a projection of Gas City lifecycle state, not a separately
// scheduled process: it is the URL the attempt's Session/worktree publishes
// (bead metadata gc.preview_url). Nothing here starts or schedules a preview;
// it only resolves and gates what the attempt already advertises, so preview
// routing stays limited to configured, safe destinations.

export const PREVIEW_URL_METADATA_KEY = 'gc.preview_url';

export type PreviewState = 'ok' | 'missing' | 'unavailable' | 'blocked';

export interface ResolvedPreview {
  state: PreviewState;
  url: string | null;
  /** Host, when a URL resolved (for display/provenance). */
  host: string | null;
}

// Default allowlist: loopback only. A deployment widens this via
// VITE_GC_PREVIEW_HOSTS (comma-separated hostnames).
const DEFAULT_ALLOWED_HOSTS = ['localhost', '127.0.0.1', '::1'];

function allowedHosts(): string[] {
  const configured = (import.meta.env as Record<string, string | undefined>).VITE_GC_PREVIEW_HOSTS;
  if (typeof configured === 'string' && configured.trim().length > 0) {
    return configured
      .split(',')
      .map((host) => host.trim().toLowerCase())
      .filter((host) => host.length > 0);
  }
  return DEFAULT_ALLOWED_HOSTS;
}

/** True when the URL is http(s) and its host is on the allowlist. */
export function isAllowedPreviewUrl(raw: string): boolean {
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    return false;
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return false;
  const host = parsed.hostname.toLowerCase();
  return allowedHosts().some((allowed) => host === allowed || host.endsWith(`.${allowed}`));
}

/**
 * Resolve the preview for a Bead's attempt. Explicit states:
 * - missing: the Bead advertises no preview URL.
 * - blocked: the URL is not on the allowlist (never framed).
 * - unavailable: the URL is malformed.
 * - ok: a safe, allowlisted URL.
 */
export function resolvePreview(bead: SupervisorBead): ResolvedPreview {
  const metadata = (bead as { metadata?: Record<string, string> }).metadata ?? {};
  const raw = (metadata[PREVIEW_URL_METADATA_KEY] ?? '').trim();
  if (raw.length === 0) return { state: 'missing', url: null, host: null };

  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    return { state: 'unavailable', url: null, host: null };
  }
  if (!isAllowedPreviewUrl(raw)) {
    return { state: 'blocked', url: null, host: parsed.hostname.toLowerCase() };
  }
  return { state: 'ok', url: parsed.toString(), host: parsed.hostname.toLowerCase() };
}
