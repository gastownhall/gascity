//go:build integration

// Package corpus loads the shared dashboard e2e fixture corpus
// (test/dashport/testdata/dashport) into an in-memory seeded city that both the
// Go serve-level integration test (Layer A, test/dashport) and the browser
// render smoke's fake supervisor (Layer B, test/dashport/cmd/fakesupervisor)
// serve. It is the ONE source of truth for the seeded scenario: a single
// scenario is asserted at both the projection level (Go) and the pixel level
// (Playwright) with no drift.
//
// The loader takes no *testing.T and returns (fixtures, error) so a main
// package can import it. The build tag keeps it out of the production binary
// and the normal integration-test surface; it compiles only under -tags
// integration, mirroring api.ServeSeededCity.
package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// Well-known ids/values the corpus seeds. Both layers assert against these, so
// they are exported here as the single source of truth: the Go projection test
// reads them directly, and the Playwright expected-strings (e2e/fixtures/
// expected.ts) mirror them. There is no automated parity check between the two —
// alignment is maintained manually, so update expected.ts whenever these change.
// Do not fork these into a second location.
const (
	// CityName is the seeded city; it is the {cityName} path segment on every
	// /v0/city/{cityName}/... and /api/city/{cityName}/... route the dashboard
	// drives.
	CityName = "dashport-city"

	// RigName is the one seeded rig the agents/rigs views project.
	RigName = "demo"

	// AnchorRunID is the seeded run root's bead id and workflow id. Both the
	// store-side /workflow/{id} read and the event-log runproj routes address
	// the run by this id, so the corpus keeps them in lockstep.
	AnchorRunID = "run-anchor"

	// AnchorStepID is the seeded in-progress step bead under the run root.
	AnchorStepID = "run-anchor.preflight"

	// AnchorStepTitle is AnchorStepID's title (its beads.json Title). It is the
	// assigned-bead title the agent-detail AgentBeadsAssigned panel renders and
	// the dependency-line title the bead-detail modal renders for the edge into
	// this step.
	AnchorStepTitle = "preflight"

	// AnchorReviewStepID is the second step under the run root, an OPEN task that
	// "needs" AnchorStepID. The bead-detail modal renders that single upstream
	// dependency ("Needs 1" → AnchorStepID · AnchorStepTitle), so it is the seeded
	// bead whose modal proves the populated BeadDependencies branch.
	AnchorReviewStepID = "run-anchor.review"

	// AnchorReviewStepTitle is AnchorReviewStepID's title; it is the bead-detail
	// modal heading when that bead's row is opened.
	AnchorReviewStepTitle = "review"

	// AnchorFormula is the seeded run's formula name; it is the run-detail
	// title the run view renders.
	AnchorFormula = "mol-adopt-pr-v2"

	// CompletedRunID is the SECOND seeded run root's bead id and workflow id: a
	// fully closed molecule (root + both steps closed, no failing gc.outcome)
	// that projects as a terminal "completed" run. It is seeded two ways from one
	// corpus — as a store-resident closed molecule AND as a bead.created →
	// bead.updated → bead.closed lifecycle in the event log capped by a
	// molecule.resolved event — so every dashboard surface renders its close-side
	// data (a historical/completed lane in the census+summary, a terminal run
	// detail, close-edge rows in the activity feed, closed rows in the beads
	// view). It is the counterpart to the happy-path in-progress AnchorRunID.
	//
	// The completed root in beads.json also carries the
	// gc.molecule_lifecycle_completed marker: a 32-hex intent id mirroring the
	// shape minted by cmd/gc/molecule_lifecycle_recovery.go on the unmerged PR
	// #4397 (not on this base branch). It is inert here — no code on this branch
	// reads it — and is seeded only so the fixture matches a real post-#4397
	// completed root. Revisit the value/shape when #4397 merges.
	CompletedRunID = "run-done"

	// CompletedFormula is the completed run's formula name; it is the completed
	// run's detail title and its run-list label. It is deliberately DISTINCT from
	// AnchorFormula so the open and completed runs are individually assertable in
	// the runs list (which labels each lane by formula name).
	CompletedFormula = "mol-review-pr-v2"

	// CompletedStepAnalyzeID and CompletedStepApproveID are the completed run's
	// two closed step beads (both status closed, closed via a bead.closed event).
	CompletedStepAnalyzeID = "run-done.analyze"
	CompletedStepApproveID = "run-done.approve"

	// SourceBeadID is the closed source task the completed run was created from;
	// the completed root carries gc.source_bead_id -> this id. It projects as a
	// closed standalone bead in the beads view.
	SourceBeadID = "src-review-1"

	// AgentName is the seeded pool agent's name; it renders in the agents view
	// as the pool members "<RigName>/<AgentName>-N".
	AgentName = "builder"

	// WorkBeadID is the seeded standalone work bead the beads view projects.
	WorkBeadID = "work-1"

	// WorkBeadTitle is the title of the seeded standalone work bead (the value
	// in testdata/dashport/beads.json for WorkBeadID); the beads view renders it.
	WorkBeadTitle = "Wire the seeded dashboard corpus"

	// MailSubject is the seeded mail message's subject the mail view projects.
	MailSubject = "seeded handoff"

	// MailFrom and MailTo are the seeded mail message's participants.
	MailFrom = "builder"
	MailTo   = "reviewer"

	// AgentSessionSlug is the seeded session's alias AND session_name; it is the
	// {slug} segment the agent-detail route resolves against (session_name → alias
	// → id, in that order). It equals AgentName so the pool agent and its live
	// session share one identity, and it matches the assignee on AnchorStepID so
	// AgentBeadsAssigned renders that in-progress bead.
	AgentSessionSlug = AgentName

	// AgentSessionTemplate is the seeded session's template ("<rig>/<agent>"). The
	// session-response rig is parsed from it (config.ParseQualifiedName), so the
	// agent-detail AgentMetadata block renders Rig = RigName from this value.
	AgentSessionTemplate = RigName + "/" + AgentName

	// AgentSessionState is the seeded session's runtime state; it is the
	// StatusBadge label the agent-detail header renders. "active" is the in-flight
	// presentation state (a non-closed session with work on its hook).
	AgentSessionState = "active"

	// OperatorMailSubject is the subject of the seeded operator↔agent thread (two
	// messages: an operator handoff and the agent's reply). It drives BOTH the
	// mail thread-detail view (a two-message thread body render) and the
	// agent-detail Chat thread pane (messages between the operator alias and the
	// seeded agent). Distinct from MailSubject so each thread is individually
	// addressable.
	OperatorMailSubject = "adopt PR #42"

	// OperatorMailFrom is the operator wire alias the dashboard treats as "me"
	// (OperatorConfig.operatorWireAlias default). The agent-detail chat pane only
	// surfaces messages between this alias (or "operator") and the agent, so the
	// seeded thread uses it as the operator participant.
	OperatorMailFrom = "human"

	// OperatorMailBody and AgentReplyBody are the two message bodies in the
	// operator↔agent thread; both the mail thread-detail render and the
	// agent-detail chat pane assert these verbatim.
	OperatorMailBody = "Please take the seeded adopt-pr run to completion and report back here."
	AgentReplyBody   = "On it. The preflight step is running now; I will report when the review step opens."
)

// Fixtures is the loaded, seeded corpus plus the stores and providers a harness
// wires into api.ServeSeededCity.
type Fixtures struct {
	CityName string
	CityPath string

	Config    *config.City
	CityStore beads.Store
	RigStores map[string]beads.Store
	EventProv events.Provider
	MailProv  *beadmail.Provider

	closeEventRecorder func() error
}

// Close drains resources the loader opened (the event-log file recorder). It is
// safe to call on a nil-recorder Fixtures and idempotent enough for a single
// deferred call. A test wraps this in t.Cleanup; the binary calls it on
// shutdown.
func (f *Fixtures) Close() error {
	if f == nil || f.closeEventRecorder == nil {
		return nil
	}
	return f.closeEventRecorder()
}

// corpusBeads is the on-disk beads.json shape: a sequence counter and the bead
// list (with explicit ids preserved verbatim in the store).
type corpusBeads struct {
	Seq   int          `json:"seq"`
	Beads []beads.Bead `json:"beads"`
}

// Load reads the corpus under dataDir (the path to the testdata/dashport
// directory), seeds an in-memory city store (beads + derived deps), replays the
// ordered event log into a FileRecorder at <cityPath>/.gc/events.jsonl (the
// exact path the host-side run tailers read), seeds one mail message, and
// returns everything a harness wires into api.ServeSeededCity.
//
// cityPath is the city root directory on disk; the caller supplies it (a test
// uses t.TempDir, the binary a scratch dir) so Load itself creates no temp
// state it cannot attribute. The returned event recorder is the SAME object
// that backs both the events feed (State.EventProvider) and the run tailer (the
// file it writes), so there is one event source of truth; call Fixtures.Close
// to drain it.
func Load(dataDir, cityPath string) (*Fixtures, error) {
	store, err := seedBeadStore(dataDir)
	if err != nil {
		return nil, err
	}
	if err := seedSession(store); err != nil {
		return nil, err
	}
	rec, closeRec, err := seedEventLog(dataDir, cityPath)
	if err != nil {
		return nil, err
	}
	mailProv, err := seedMail(store)
	if err != nil {
		_ = closeRec()
		return nil, err
	}

	return &Fixtures{
		CityName:           CityName,
		CityPath:           cityPath,
		Config:             corpusConfig(),
		CityStore:          store,
		RigStores:          map[string]beads.Store{RigName: beads.NewMemStore()},
		EventProv:          rec,
		MailProv:           mailProv,
		closeEventRecorder: closeRec,
	}, nil
}

// seedBeadStore loads beads.json and returns a MemStore that preserves the
// corpus bead ids and derives parent/needs dependencies, so /beads,
// /workflow/{id}, and /mail all project the real topology.
func seedBeadStore(dataDir string) (beads.Store, error) {
	raw, err := readCorpus(dataDir, "beads.json")
	if err != nil {
		return nil, err
	}
	var cb corpusBeads
	if err := json.Unmarshal(raw, &cb); err != nil {
		return nil, fmt.Errorf("decode beads.json: %w", err)
	}

	deps := make([]beads.Dep, 0)
	for _, b := range cb.Beads {
		// A step "needs" its predecessor; the workflow snapshot walks DepList
		// down (IssueID == this bead) and emits from=DependsOnID → to=IssueID.
		for _, need := range b.Needs {
			depType, dependsOnID := "blocks", need
			if kind, id, ok := strings.Cut(need, ":"); ok && kind != "" && id != "" {
				depType, dependsOnID = kind, id
			}
			deps = append(deps, beads.Dep{IssueID: b.ID, DependsOnID: dependsOnID, Type: depType})
		}
	}

	return beads.NewMemStoreFrom(cb.Seq, cb.Beads, deps), nil
}

// seedEventLog replays events.jsonl (in file order) through a FileRecorder at
// <cityPath>/.gc/events.jsonl. Record auto-assigns the seq in call order, so
// the corpus order defines the projected seq order for both the events feed and
// the runproj fold. It returns the recorder (as an events.Provider) plus a
// close func the caller drains.
func seedEventLog(dataDir, cityPath string) (events.Provider, func() error, error) {
	logPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	rec, err := events.NewFileRecorder(logPath, os.Stderr)
	if err != nil {
		return nil, nil, fmt.Errorf("new file recorder %s: %w", logPath, err)
	}

	raw, err := readCorpus(dataDir, "events.jsonl")
	if err != nil {
		_ = rec.Close()
		return nil, nil, err
	}
	for _, line := range splitNonEmptyLines(raw) {
		var e events.Event
		if err := json.Unmarshal(line, &e); err != nil {
			_ = rec.Close()
			return nil, nil, fmt.Errorf("decode event %q: %w", truncate(line), err)
		}
		// Let the recorder assign seq AND envelope Ts in append order: the corpus
		// seqs are documentation of intended order (not authoritative), and zeroing
		// the ENVELOPE Ts makes the FileRecorder stamp time.Now() (recorder.go).
		// Recent envelope timestamps are what let the Activity view — whose default
		// window is the last 24h — render the seeded event rows; the fixed
		// 2026-06-01 corpus dates would otherwise fall outside every selectable
		// window. The runproj/workflow projections Layer A asserts are
		// recency-agnostic (they key on presence + status), so this does not perturb
		// the Go serve-level assertions.
		//
		// Only the ENVELOPE Ts is re-stamped. Timestamps embedded in the payload —
		// each bead snapshot's created_at/updated_at and the molecule.resolved
		// payload's own ts — are left as the fixed scenario values on purpose: they
		// stay mutually consistent (the completed run's created_at→updated_at span,
		// and its molecule.resolved ts == the root's close updated_at), so any
		// duration/close-time derived from the payload is coherent while the
		// activity-window filter keys off the re-stamped envelope Ts. The events are
		// ordered in true scenario chronology (the earlier completed run first, then
		// the later in-progress run) so the appended seq order matches the payload
		// timeline.
		e.Seq = 0
		e.Ts = time.Time{}
		rec.Record(e)
	}
	return rec, rec.Close, nil
}

// seedSession creates one active session bead in the city store so the sessions
// list projects a live agent the agent-detail view (/agents/{slug}) can resolve.
// Without a matching session, that route renders only its not-found shell.
//
// The session's alias/session_name is AgentSessionSlug (== AgentName), which is
// both the route slug and the assignee on the in-progress AnchorStepID bead, so
// AgentBeadsAssigned renders a real in-flight assignment. Its template encodes
// the rig (config.ParseQualifiedName → RigName) for the AgentMetadata block, and
// the operator↔agent thread seedMail sends drives the chat pane.
func seedSession(store beads.Store) error {
	sessStore := session.NewStore(beads.SessionStore{Store: store})
	if _, err := sessStore.CreateSessionInfo(session.CreateSpec{
		Title:     AgentName,
		AgentName: AgentName,
		Metadata: map[string]string{
			"alias":        AgentSessionSlug,
			"session_name": AgentSessionSlug,
			"template":     AgentSessionTemplate,
			"provider":     "test-agent",
			"state":        AgentSessionState,
		},
	}); err != nil {
		return fmt.Errorf("seed session: %w", err)
	}
	return nil
}

// seedMail sends messages through the city bead store's mail provider so the
// /mail feed, a thread read, and the agent-detail chat pane project real message
// beads. Two threads are seeded:
//   - a single builder→reviewer handoff (MailSubject), the mail-list row; and
//   - a two-message operator↔agent thread (OperatorMailSubject): an operator
//     handoff plus the agent's reply, sharing one thread label. The pair backs
//     the mail thread-detail render (both bodies) and the agent-detail chat pane
//     (messages between the operator alias and the seeded agent).
func seedMail(store beads.Store) (*beadmail.Provider, error) {
	mp := beadmail.New(store)
	if _, err := mp.Send(MailFrom, MailTo, MailSubject, "please adopt the seeded PR"); err != nil {
		return nil, fmt.Errorf("seed mail: %w", err)
	}
	handoff, err := mp.Send(OperatorMailFrom, AgentSessionSlug, OperatorMailSubject, OperatorMailBody)
	if err != nil {
		return nil, fmt.Errorf("seed operator handoff mail: %w", err)
	}
	if _, err := mp.Reply(handoff.ID, AgentSessionSlug, OperatorMailSubject, AgentReplyBody); err != nil {
		return nil, fmt.Errorf("seed agent reply mail: %w", err)
	}
	return mp, nil
}

// corpusConfig builds the seeded city config in Go (config.City uses TOML tags,
// so it is authored here rather than deserialized from the corpus). It mirrors
// the fake-state defaults but names one rig and one agent the assertions
// expect.
func corpusConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: CityName},
		Agents: []config.Agent{
			{Name: AgentName, Dir: RigName, Provider: "test-agent", MaxActiveSessions: intPtr(2)},
		},
		Rigs: []config.Rig{
			{Name: RigName, Path: filepath.Join(os.TempDir(), "dashport-"+RigName)},
		},
		Providers: map[string]config.ProviderSpec{
			"test-agent": {DisplayName: "Test Agent"},
		},
	}
}

// readCorpus reads a named corpus file under dataDir, wrapping the path in the
// error for a self-describing failure.
func readCorpus(dataDir, name string) ([]byte, error) {
	path := filepath.Join(dataDir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read corpus %s: %w", path, err)
	}
	return raw, nil
}

func splitNonEmptyLines(raw []byte) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, []byte(line))
	}
	return out
}

func truncate(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

func intPtr(n int) *int { return &n }
