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

	// AnchorFormula is the seeded run's formula name; it is the run-detail
	// title the run view renders.
	AnchorFormula = "mol-adopt-pr-v2"

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
		// Let the recorder assign seq AND ts in append order: the corpus seqs are
		// documentation of intended order (not authoritative), and zeroing Ts makes
		// the FileRecorder stamp time.Now() (recorder.go). Recent timestamps are
		// what let the Activity view — whose default window is the last 24h — render
		// the seeded event rows; the fixed 2026-06-01 corpus dates would otherwise
		// fall outside every selectable window. The runproj/workflow projections
		// Layer A asserts are recency-agnostic (they key on presence + status), so
		// this does not perturb the Go serve-level assertions.
		e.Seq = 0
		e.Ts = time.Time{}
		rec.Record(e)
	}
	return rec, rec.Close, nil
}

// seedMail sends one message through the city bead store's mail provider so the
// /mail feed and a thread read project a real message bead.
func seedMail(store beads.Store) (*beadmail.Provider, error) {
	mp := beadmail.New(store)
	if _, err := mp.Send(MailFrom, MailTo, MailSubject, "please adopt the seeded PR"); err != nil {
		return nil, fmt.Errorf("seed mail: %w", err)
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
