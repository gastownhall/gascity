package runproj

// RunSummary is the dashboard run-view DTO, a faithful Go port of the
// TypeScript RunSummary in
// internal/api/dashboardspa/web/shared/src/snapshot/types.ts. Field order here
// is load-bearing: the golden-parity test marshals this struct with the same
// canonical JSON the TS generator used (JSON.stringify(..., 2)), so the JSON
// key order must match the TS object-literal key order in
// shared/src/runs/summary.ts (buildRunSummary).
type RunSummary struct {
	// TotalActive counts ACTIVE lanes (phase neither "complete" nor "blocked").
	TotalActive int `json:"totalActive"`
	// TotalHistorical is the TRUE count of completed lanes (may exceed
	// len(HistoricalLanes) once the cap applies).
	TotalHistorical int            `json:"totalHistorical"`
	RunCounts       RunCounts      `json:"runCounts"`
	Lanes           []RunLane      `json:"lanes"`
	HistoricalLanes []RunLane      `json:"historicalLanes"`
	BlockedLanes    []RunLane      `json:"blockedLanes"`
	RecentChanges   []RunChange    `json:"recentChanges"`
	Census          RunCensusState `json:"census"`
	// LanesPartial is set only when the builder ran in partial mode; omitted
	// otherwise (TS optional literal `true`).
	LanesPartial bool `json:"lanesPartial,omitempty"`
}

// RunCounts is the per-kind lane tally. Port of TS RunCounts.
type RunCounts struct {
	Total int `json:"total"`
	// Visible equals Total (RunMap owns the rendered collapse; deprecated in TS).
	Visible      int `json:"visible"`
	PrReview     int `json:"prReview"`
	DesignReview int `json:"designReview"`
	Bugfix       int `json:"bugfix"`
	Blocked      int `json:"blocked"`
	Other        int `json:"other"`
}

// RunLane is a single run lane. Port of TS RunLane. Field order matches the
// object literal returned by runLane() in summary.ts.
type RunLane struct {
	ID              string                   `json:"id"`
	Title           string                   `json:"title"`
	Formula         RunLaneFormula           `json:"formula"`
	Scope           RunLaneScope             `json:"scope"`
	External        RunLaneExternalReference `json:"external"`
	Phase           string                   `json:"phase"`
	PhaseLabel      string                   `json:"phaseLabel"`
	StatusCounts    StatusCounts             `json:"statusCounts"`
	ActiveAssignees []string                 `json:"activeAssignees"`
	UpdatedAt       RunLaneUpdatedAt         `json:"updatedAt"`
	Stages          []RunStage               `json:"stages"`
	Progress        RunLaneProgress          `json:"progress"`
	// FormulaStageResolved is true when stages came from a recognized formula
	// AND the active gc.step_id mapped into one of those formula stages.
	FormulaStageResolved bool               `json:"formulaStageResolved"`
	Health               RunLaneHealthState `json:"health"`
}

// RunLaneFormula is the discriminated formula-identity union. TS RunLaneFormula:
// {status:'known', name} | {status:'unavailable', error}. Marshaled via a custom
// MarshalJSON so only the active arm's fields appear.
type RunLaneFormula struct {
	Status string // "known" | "unavailable"
	Name   string
	Error  string
}

// RunLaneScope is the lane scope union. TS Avail<{kind, ref, rootStoreRef}>:
// {status:'available', kind, ref, rootStoreRef} | {status:'unavailable', error}.
type RunLaneScope struct {
	Status       string // "available" | "unavailable"
	Kind         string
	Ref          string
	RootStoreRef string
	Error        string
}

// RunLaneExternalReference is the external-reference union. TS:
// {status:'available', label, url} | {status:'label_only', label} |
// {status:'unavailable', error}.
type RunLaneExternalReference struct {
	Status string // "available" | "label_only" | "unavailable"
	Label  string
	URL    string
	Error  string
}

// RunLaneUpdatedAt is the updated-at union. TS Avail<{at}>.
type RunLaneUpdatedAt struct {
	Status string // "available" | "unavailable"
	At     string
	Error  string
}

// StatusCounts is a per-status tally that preserves first-seen status order, so
// it serializes in the same key order the TS `Record<string, number>` does (JS
// objects keep insertion order; a Go map would sort keys and break parity).
type StatusCounts struct {
	keys   []string
	counts map[string]int
}

// inc records one occurrence of status, tracking first-seen order.
func (s *StatusCounts) inc(status string) {
	if s.counts == nil {
		s.counts = map[string]int{}
	}
	if _, ok := s.counts[status]; !ok {
		s.keys = append(s.keys, status)
	}
	s.counts[status]++
}

// MarshalJSON renders the counts in first-seen order.
func (s StatusCounts) MarshalJSON() ([]byte, error) {
	pairs := make([]kv, 0, len(s.keys))
	for _, k := range s.keys {
		pairs = append(pairs, kv{k, s.counts[k]})
	}
	return marshalObject(pairs)
}

// RunStage is one stage in a lane's ladder. Port of TS RunStage.
type RunStage struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"` // "pending" | "active" | "complete" | "blocked"
}

// RunLaneProgress is the lane-progress union. TS RunLaneProgress:
// {status:'active_step', stepId, stage, attempt} |
// {status:'stage_only', stage, error} | {status:'unavailable', error}.
type RunLaneProgress struct {
	Status  string // "active_step" | "stage_only" | "unavailable"
	StepID  string
	Stage   RunLaneStagePosition
	Attempt RunLaneStepAttempt
	Error   string
}

// RunLaneStagePosition is the active-stage union. TS Avail<{index, key, label}>.
type RunLaneStagePosition struct {
	Status string // "available" | "unavailable"
	Index  int
	Key    string
	Label  string
	Error  string
}

// RunLaneStepAttempt is the step-attempt union. TS Avail<{value}>.
type RunLaneStepAttempt struct {
	Status string // "available" | "unavailable"
	Value  int
	Error  string
}

// RunLaneHealthState is the lane-health union. TS Avail<{data}>. BuildRunSummary
// emits the unavailable arm; EnrichRunSummary replaces it with the available arm
// carrying the derived RunLaneHealth.
type RunLaneHealthState struct {
	Status string // "available" | "unavailable"
	Data   RunLaneHealth
	Error  string
}

// RunLaneHealth is the engine-derived per-lane health. Port of TS RunLaneHealth.
// Field order matches the object literal built in deriveRunHealth (health.ts).
type RunLaneHealth struct {
	PhaseConfidence   string // "known" | "inferred"
	NeedsOperator     bool
	StuckNode         RunLaneStuckNode
	ThrashingDetected bool
	Session           RunLaneSessionState
}

// RunLaneStuckNode is the stuck-node union. TS Avail<{id}>.
type RunLaneStuckNode struct {
	Status string // "available" | "unavailable"
	ID     string
	Error  string
}

// RunLaneSessionState is the resolved-session union. TS RunLaneSessionState:
// {status:'resolved', lastActive, running, activity} | {status:'unresolved', error}.
type RunLaneSessionState struct {
	Status     string // "resolved" | "unresolved"
	LastActive RunLaneSessionLastActive
	Running    RunLaneSessionRunning
	Activity   RunLaneSessionActivity
	Error      string
}

// RunLaneSessionLastActive is the last-active union. TS Avail<{at}>.
type RunLaneSessionLastActive struct {
	Status string // "available" | "unavailable"
	At     string
	Error  string
}

// RunLaneSessionRunning is the running-flag union. TS Avail<{value}>.
type RunLaneSessionRunning struct {
	Status string // "available" | "unavailable"
	Value  bool
	Error  string
}

// RunLaneSessionActivity is the activity-hint union. TS Avail<{value}>.
type RunLaneSessionActivity struct {
	Status string // "available" | "unavailable"
	Value  string
	Error  string
}

// RunCensusState is the city-census union. TS Avail<{data}>. BuildRunSummary
// emits the unavailable arm; EnrichRunSummary replaces it with the available arm
// carrying the derived RunCensus.
type RunCensusState struct {
	Status string // "available" | "unavailable"
	Data   RunCensus
	Error  string
}

// RunCensus is the threshold-independent city census. Port of TS RunCensus.
// Field order matches the object literal built in buildCensus (health.ts).
type RunCensus struct {
	ByPhase          RunCensusByPhase
	TotalInFlight    int
	Unverifiable     int
	KnownDenominator int
	Thrashing        int
}

// RunCensusByPhase is the per-phase lane tally. Port of TS Record<RunPhase,number>.
// Key order matches zeroByPhase() in health.ts (NOT alphabetical).
type RunCensusByPhase struct {
	Intake         int
	Implementation int
	Review         int
	Approval       int
	Finalization   int
	Blocked        int
	Complete       int
	Active         int
}

// RunChange is one recent-change row. Port of TS RunChange.
type RunChange struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updatedAt"`
}
