// Expected strings for the Playwright render smoke, mirroring the shared corpus
// seeded by test/dashport/corpus (the Go loader) into
// test/dashport/testdata/dashport. This is the Layer B copy of the corpus
// ids/values; the Go side asserts against test/dashport/corpus's exported
// constants directly.
//
// There is NO automated parity check between this file and corpus.go —
// alignment is maintained MANUALLY. When you change a value below, change the
// matching exported constant in test/dashport/corpus/corpus.go (and vice
// versa), or the browser will assert against content the seeded server no
// longer serves. The constant mapping is:
//
//   CITY_NAME        <-> corpus.CityName
//   RIG_NAME         <-> corpus.RigName
//   ANCHOR_RUN_ID    <-> corpus.AnchorRunID
//   ANCHOR_FORMULA   <-> corpus.AnchorFormula
//   WORK_BEAD_ID     <-> corpus.WorkBeadID
//   WORK_BEAD_TITLE  <-> corpus.WorkBeadTitle
//   MAIL_SUBJECT     <-> corpus.MailSubject
//   AGENT_NAME       <-> corpus.AgentName

export const CITY_NAME = 'dashport-city';
export const RIG_NAME = 'demo';

/** The seeded run root's bead id and workflow id. */
export const ANCHOR_RUN_ID = 'run-anchor';

/** The seeded run's formula name — the run-detail title and the runs-list label. */
export const ANCHOR_FORMULA = 'mol-adopt-pr-v2';

/** The seeded standalone work bead the beads view lists. */
export const WORK_BEAD_ID = 'work-1';
export const WORK_BEAD_TITLE = 'Wire the seeded dashboard corpus';

/** The seeded mail message the mail view lists. */
export const MAIL_SUBJECT = 'seeded handoff';

/** The seeded agent name (from the corpus config). */
export const AGENT_NAME = 'builder';

/** Base path for the seeded city's client routes (BrowserRouter basename). */
export const CITY_BASE = `/city/${CITY_NAME}`;

/**
 * The endpoint the SPA POSTs client errors to (lib/clientErrorReporting.ts). A
 * spec fails if the browser hits this while rendering a seeded view — it means a
 * render threw and the error boundary caught it.
 */
export const CLIENT_ERROR_ENDPOINT = '/api/client-errors';

/**
 * Text rendered by components/ErrorBoundary.tsx's crash fallback. A spec asserts
 * this is NOT present on any seeded route.
 */
export const ERROR_BOUNDARY_TEXT = 'Dashboard view failed.';
