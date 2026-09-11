/** Wrapped error returned by the backend on any 4xx/5xx. */
export interface ApiError {
  error: string;
  /** Optional machine-readable kind (e.g. "validation", "not_found"). */
  kind?: string;
  /** Optional details object — never leaks raw stderr to the browser. */
  details?: Record<string, string>;
  /**
   * Optional run-detail discriminator. The BFF `/runs/{id}/detail` endpoint
   * sets this on a 422 to `'not_run_view'` (an honest list-only v1/wisp run)
   * or `'invalid_snapshot'` (a genuine load failure) so the SPA can render the
   * two unprocessable cases distinctly. Other endpoints leave it absent.
   */
  reason?: string;
}
