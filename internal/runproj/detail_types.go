package runproj

// The run-detail DTO is a faithful Go port of the TypeScript FormulaRunDetail
// (internal/api/dashboardspa/web/shared/src/run-detail.ts) and the run-snapshot
// input shape (run-snapshot.ts). BuildRunDetail folds a city's beads into one
// run's detail graph, the SAME projection the supervisor's /workflow/{id} route
// produced client-side. Field order in these structs is load-bearing: the
// golden-parity test marshals them with the same canonical JSON the TS generator
// used (JSON.stringify(..., 2)), so the JSON key order must match the TS
// object-literal key order the detail pipeline emits.

// runSnapshotDep is a raw dependency edge from a run snapshot. Port of TS
// RunSnapshotDep. Kept package-internal: BuildRunDetail synthesizes deps from the
// folded beads (the supervisor's RunSnapshot is not on the OSS-local path).
type runSnapshotDep struct {
	from string
	to   string
	kind string
}

// runSnapshotBead is one bead row inside a run snapshot — supervisor wire shape,
// not the dashboard display node. Port of TS RunSnapshotBead. Package-internal:
// it is projected from beads.Bead by toRunSnapshotBead.
type runSnapshotBead struct {
	id            string
	title         string
	status        string
	kind          string
	stepRef       string
	attempt       *int
	logicalBeadID string
	scopeRef      string
	assignee      string
	metadata      map[string]string
}

// runSnapshot is the dashboard-normalized supervisor snapshot. Port of TS
// RunSnapshot. BuildRunDetail synthesizes it from the folded beads, mirroring the
// golden generator's snapshotForRun.
type runSnapshot struct {
	runID             string
	rootBeadID        string
	rootStoreRef      string
	resolvedRootStore string
	scopeKind         string
	scopeRef          string
	snapshotVersion   int
	snapshotEventSeq  *int64
	partial           bool
	storesScanned     []string
	beads             []runSnapshotBead
	deps              []runSnapshotDep
	logicalEdges      []runSnapshotDep
}

// RunFormula is the run's formula-identity union. TS RunFormula:
// {kind:'known', name, source} | {kind:'unavailable', reason}.
type RunFormula struct {
	Kind   string // "known" | "unavailable"
	Name   string
	Source string // "metadata" | "title_fallback"
	Reason string // "missing_formula_metadata"
}

// RunFormulaDetailState is the compiled-formula-detail union. TS
// RunFormulaDetailState (four arms).
type RunFormulaDetailState struct {
	Kind    string // "available" | "unavailable"
	Name    string
	Target  string
	Reason  string // "missing_formula_metadata" | "missing_run_target" | "fetch_failed"
	Failure string // RunFormulaDetailFetchFailure (fetch_failed arm only)
}

// RunExecutionPath is the run-diff execution-path union. TS RunExecutionPath:
// {kind:'known', path} | {kind:'unavailable', reason}.
type RunExecutionPath struct {
	Kind   string // "known" | "unavailable"
	Path   string
	Reason string // "missing_cwd_and_rig_root"
}

// RunSnapshotSequence is the snapshot-event-seq union. TS RunSnapshotSequence:
// {kind:'known', seq} | {kind:'unavailable', reason:'supervisor_omitted'}.
type RunSnapshotSequence struct {
	Kind   string // "known" | "unavailable"
	Seq    int64
	Reason string // "supervisor_omitted"
}

// FormulaRunCompleteness is the completeness union. TS FormulaRunCompleteness:
// {kind:'complete'} | {kind:'partial', reasons}.
type FormulaRunCompleteness struct {
	Kind    string // "complete" | "partial"
	Reasons []string
}

// RunNodeScope is the per-node scope union. TS RunNodeScope:
// {kind:'run'} | {kind:'scoped', ref}.
type RunNodeScope struct {
	Kind string // "run" | "scoped"
	Ref  string
}

// RunIteration is the loop-iteration union. TS RunIteration:
// {kind:'base'} | {kind:'loop', value}.
type RunIteration struct {
	Kind  string // "base" | "loop"
	Value int
}

// RunAttempt is the attempt union. TS RunAttempt:
// {kind:'untracked'} | {kind:'attempt', value}.
type RunAttempt struct {
	Kind  string // "untracked" | "attempt"
	Value int
}

// RunSessionLink is a resolved session reference. Port of TS RunSessionLink.
type RunSessionLink struct {
	SessionID   string `json:"sessionId"`
	SessionName string `json:"sessionName"`
	Assignee    string `json:"assignee"`
}

// RunSessionAttachment is the per-instance session union. TS
// RunSessionAttachment:
// {kind:'attached', link, streamable} | {kind:'none', reason}.
type RunSessionAttachment struct {
	Kind       string // "attached" | "none"
	Link       RunSessionLink
	Streamable bool
	Reason     string // "not_started" | "session_unresolved"
}

// RunIterationSummary is the node-level iteration summary union. TS
// RunIterationSummary: {kind:'single'} | {kind:'stacked', visibleIteration,
// iterationCount, control}.
type RunIterationSummary struct {
	Kind             string // "single" | "stacked"
	VisibleIteration int
	IterationCount   int
	Control          RunIterationControl
}

// RunIterationControl is the stacked-summary control union. TS:
// {kind:'known', id} | {kind:'unknown'}.
type RunIterationControl struct {
	Kind string // "known" | "unknown"
	ID   string
}

// RunAttemptSummary is the node-level attempt summary union. TS
// RunAttemptSummary: {kind:'none'} | {kind:'tracked', count, badge, active}.
type RunAttemptSummary struct {
	Kind   string // "none" | "tracked"
	Count  int
	Badge  RunAttemptBadge
	Active RunAttemptActive
}

// RunAttemptBadge is the tracked-summary badge union. TS:
// {kind:'bounded', label} | {kind:'count-only'}.
type RunAttemptBadge struct {
	Kind  string // "bounded" | "count-only"
	Label string
}

// RunAttemptActive is the tracked-summary active union. TS:
// {kind:'running', value} | {kind:'idle'}.
type RunAttemptActive struct {
	Kind  string // "running" | "idle"
	Value int
}

// RunExecutionInstance is one physical bead execution behind a semantic node.
// Port of TS RunExecutionInstance. Field order matches the object literal in
// buildExecutionInstance (execution-instances.ts).
type RunExecutionInstance struct {
	ID               string               `json:"id"`
	SemanticNodeID   string               `json:"semanticNodeId"`
	BeadID           string               `json:"beadId"`
	Iteration        RunIteration         `json:"iteration"`
	Attempt          RunAttempt           `json:"attempt"`
	Label            string               `json:"label"`
	Status           string               `json:"status"`
	Session          RunSessionAttachment `json:"session"`
	CurrentIteration bool                 `json:"currentIteration"`
	Historical       bool                 `json:"historical"`
}

// RunControlBadge is a hidden-construct badge attached to a visible node. Port of
// TS RunControlBadge.
type RunControlBadge struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
}

// RunDisplayNode is one semantic node in the run graph. Port of TS
// RunDisplayNode. Field order matches the run-detail.ts interface (the object
// literal in buildRunDisplayNode emits the same order).
type RunDisplayNode struct {
	ID                         string                 `json:"id"`
	SemanticNodeID             string                 `json:"semanticNodeId"`
	Title                      string                 `json:"title"`
	Kind                       string                 `json:"kind"`
	ConstructKind              string                 `json:"constructKind"`
	Status                     string                 `json:"status"`
	CurrentBeadID              string                 `json:"currentBeadId"`
	Scope                      RunNodeScope           `json:"scope"`
	VisibleInGraph             bool                   `json:"visibleInGraph"`
	HistoricalOnly             bool                   `json:"historicalOnly"`
	IterationSummary           RunIterationSummary    `json:"iterationSummary"`
	AttemptSummary             RunAttemptSummary      `json:"attemptSummary"`
	VisibleExecutionInstanceID string                 `json:"visibleExecutionInstanceId"`
	ExecutionInstances         []RunExecutionInstance `json:"executionInstances"`
	ControlBadges              []RunControlBadge      `json:"controlBadges"`
}

// RunDisplayEdge is a directed edge between semantic nodes. Port of TS
// RunDisplayEdge.
type RunDisplayEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// RunDisplayLane groups nodes by scope. Port of TS RunDisplayLane.
type RunDisplayLane struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	NodeIDs []string `json:"nodeIds"`
}

// FormulaRunProgress is the run progress/census summary. Port of TS
// FormulaRunProgress. Field order matches buildFormulaRunProgress (formula-run.ts).
type FormulaRunProgress struct {
	SnapshotVersion        int                 `json:"snapshotVersion"`
	SnapshotEventSeq       RunSnapshotSequence `json:"snapshotEventSeq"`
	SnapshotPartial        bool                `json:"snapshotPartial"`
	TotalNodeCount         int                 `json:"totalNodeCount"`
	VisibleNodeCount       int                 `json:"visibleNodeCount"`
	EdgeCount              int                 `json:"edgeCount"`
	ExecutionInstanceCount int                 `json:"executionInstanceCount"`
	SessionLinkCount       int                 `json:"sessionLinkCount"`
	StreamableSessionCount int                 `json:"streamableSessionCount"`
	StreamableSessionIDs   []string            `json:"streamableSessionIds"`
	StatusCounts           nodeStatusCounts    `json:"statusCounts"`
	AllStatusCounts        nodeStatusCounts    `json:"allStatusCounts"`
	// Terminal reports whether every visible node has reached a terminal
	// status. It is the single source of the status-terminality taxonomy
	// (see terminalRunNodeStatuses / nonTerminalRunNodeStatuses); the client
	// renders it instead of re-deriving the taxonomy in TypeScript.
	Terminal bool `json:"terminal"`
}

// FormulaRunDetail is the run-detail DTO. Port of TS FormulaRunDetail. Field
// order matches the object literal enrichFormulaRun returns (enrich.ts).
type FormulaRunDetail struct {
	RunID             string                 `json:"runId"`
	RootBeadID        string                 `json:"rootBeadId"`
	RootStoreRef      string                 `json:"rootStoreRef"`
	ResolvedRootStore string                 `json:"resolvedRootStore"`
	ScopeKind         string                 `json:"scopeKind"`
	ScopeRef          string                 `json:"scopeRef"`
	Title             string                 `json:"title"`
	Formula           RunFormula             `json:"formula"`
	FormulaDetail     RunFormulaDetailState  `json:"formulaDetail"`
	ExecutionPath     RunExecutionPath       `json:"executionPath"`
	SnapshotVersion   int                    `json:"snapshotVersion"`
	SnapshotEventSeq  RunSnapshotSequence    `json:"snapshotEventSeq"`
	Completeness      FormulaRunCompleteness `json:"completeness"`
	Progress          FormulaRunProgress     `json:"progress"`
	Phase             string                 `json:"phase"`
	Stages            []RunStage             `json:"stages"`
	Nodes             []RunDisplayNode       `json:"nodes"`
	Edges             []RunDisplayEdge       `json:"edges"`
	Lanes             []RunDisplayLane       `json:"lanes"`
}
