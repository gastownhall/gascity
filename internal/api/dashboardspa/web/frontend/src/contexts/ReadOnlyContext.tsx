import { createContext, useContext, type ReactNode } from 'react';
import { StatusBadge } from '../components/StatusBadge';

// Server-enforced read-only posture surfaced to the SPA
// (gascity-dashboard-uzhr). When the backend runs with `DASHBOARD_READONLY=1`,
// the supervisor transport-proxy gate (z8n7) 405s every mutation. Without a
// matching client affordance the SPA's create/sling/claim/close/nudge buttons
// stay live and a click 405s into an unhandled API error. This context carries
// the flag from `/config` so mutating controls render DISABLED (per
// DESIGN.md §States have words: disabled + an explanatory title), never hidden.
//
// Default `false` covers the pre-config-load window and any consumer mounted
// outside the provider: controls render enabled, exactly as before this flag
// existed. The server gate remains the real enforcement, so the only risk in
// that sub-second window is the same 405 the affordance removes once config
// lands — never a privilege grant.

const ReadOnlyContext = createContext<boolean>(false);

export function ReadOnlyProvider({
  readOnly,
  children,
}: {
  readOnly: boolean;
  children: ReactNode;
}) {
  return <ReadOnlyContext.Provider value={readOnly}>{children}</ReadOnlyContext.Provider>;
}

/** True when the dashboard backend is in read-only mode (DASHBOARD_READONLY=1). */
export function useReadOnly(): boolean {
  return useContext(ReadOnlyContext);
}

/**
 * Resolve the SPA's read-only posture from the `/config` fetch state. Three
 * cases, because "config not yet decoded" is not the same as "config says
 * writable" (gascity-dashboard-uzhr):
 *   - config decoded   -> trust its `readOnly` flag.
 *   - config errored    -> fail CLOSED (read-only). A network failure, or a
 *     backend too old to emit `readOnly` (the edge decoder then throws), must
 *     NOT leave every mutating control live to 405 on click — that is exactly
 *     the regression this affordance removes.
 *   - config in flight  -> writable, matching prior behaviour, so controls
 *     don't flash disabled on every normal page load. The window is sub-second
 *     and the server proxy gate is the real enforcement throughout.
 */
export function resolveReadOnly(
  config: { readOnly: boolean } | undefined,
  configError: string | null,
): boolean {
  if (config) return config.readOnly;
  return configError !== null;
}

/** Title/affordance copy for a control disabled by read-only mode. Colon, not
 *  an em dash, per DESIGN.md §"Don't use em dashes in UI copy". */
export const READ_ONLY_CONTROL_TITLE = 'Read-only mode: mutations are disabled';

/** The single "Read-only" affordance badge shared by every disabled mutating
 *  control (DESIGN.md §States have words: a glyph + word, not color alone).
 *  Render behind a `readOnly &&` guard. */
export function ReadOnlyBadge() {
  return <StatusBadge tone="warn" label="Read-only" title={READ_ONLY_CONTROL_TITLE} />;
}
