// The run-summary FOLD (buildRunSummary, counts, lane/scope/phase derivation,
// enrichment) moved to Go (internal/runproj); the dashboard renders the
// server-computed RunSummary DTO. This window size is the one presentation knob
// the renderer keeps: the wire carries the FULL active set in `lanes`, and
// RunMap renders this many by default behind a "Show N more runs" expander.
export const MAX_VISIBLE_ACTIVE_LANES = 8;
