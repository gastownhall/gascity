//go:build integration

package dashport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

const (
	corpusCityName = "dashport-city"
	corpusRigName  = "demo"

	// anchorRunID is the seeded run root's bead id and workflow id. Both the
	// store-side /workflow/{id} read and the event-log runproj routes address the
	// run by this id, so the corpus keeps them in lockstep.
	anchorRunID       = "run-anchor"
	anchorStepID      = "run-anchor.preflight"
	anchorFormula     = "mol-adopt-pr-v2"
	corpusWorkBeadID  = "work-1"
	corpusMailSubject = "seeded handoff"
	corpusMailFrom    = "builder"
	corpusMailTo      = "reviewer"
)

// fixtures is the loaded, seeded corpus plus the state a test drives.
type fixtures struct {
	CityName string
	CityPath string

	config    *config.City
	cityStore beads.Store
	rigStores map[string]beads.Store
	eventProv events.Provider
	mailProv  *beadmail.Provider
}

// corpusBeads is the on-disk beads.json shape: a sequence counter and the bead
// list (with explicit ids preserved verbatim in the store).
type corpusBeads struct {
	Seq   int          `json:"seq"`
	Beads []beads.Bead `json:"beads"`
}

// loadFixtures reads testdata/dashport, seeds an in-memory city store (beads +
// derived deps), replays the ordered event log into a FileRecorder at
// <cityPath>/.gc/events.jsonl (the exact path the host-side run tailers read),
// seeds one mail message, and returns everything the harness wires into
// api.ServeSeededCity. The event recorder is the SAME object that backs both the
// events feed (State.EventProvider) and the run tailer (the file it writes), so
// there is one event source of truth.
func loadFixtures(t *testing.T) *fixtures {
	t.Helper()

	cityPath := t.TempDir()

	store := seedBeadStore(t)
	rec := seedEventLog(t, cityPath)
	mailProv := seedMail(t, store)

	return &fixtures{
		CityName:  corpusCityName,
		CityPath:  cityPath,
		config:    corpusConfig(),
		cityStore: store,
		rigStores: map[string]beads.Store{corpusRigName: beads.NewMemStore()},
		eventProv: rec,
		mailProv:  mailProv,
	}
}

// seedBeadStore loads beads.json and returns a MemStore that preserves the
// corpus bead ids and derives parent/needs dependencies, so /beads,
// /workflow/{id}, and /mail all project the real topology.
func seedBeadStore(t *testing.T) beads.Store {
	t.Helper()

	raw := readCorpus(t, "beads.json")
	var cb corpusBeads
	if err := json.Unmarshal(raw, &cb); err != nil {
		t.Fatalf("decode beads.json: %v", err)
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

	return beads.NewMemStoreFrom(cb.Seq, cb.Beads, deps)
}

// seedEventLog replays events.jsonl (in file order) through a FileRecorder at
// <cityPath>/.gc/events.jsonl. Record auto-assigns the seq in call order, so the
// corpus order defines the projected seq order for both the events feed and the
// runproj fold.
func seedEventLog(t *testing.T, cityPath string) events.Provider {
	t.Helper()

	logPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	rec, err := events.NewFileRecorder(logPath, os.Stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder(%s): %v", logPath, err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	for _, line := range splitNonEmptyLines(readCorpus(t, "events.jsonl")) {
		var e events.Event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("decode event %q: %v", truncate(line), err)
		}
		// Let the recorder assign seq/ts in append order; the corpus seqs are
		// documentation of intended order, not authoritative.
		e.Seq = 0
		rec.Record(e)
	}
	return rec
}

// seedMail sends one message through the city bead store's mail provider so the
// /mail feed and a thread read project a real message bead.
func seedMail(t *testing.T, store beads.Store) *beadmail.Provider {
	t.Helper()
	mp := beadmail.New(store)
	if _, err := mp.Send(corpusMailFrom, corpusMailTo, corpusMailSubject, "please adopt the seeded PR"); err != nil {
		t.Fatalf("seed mail: %v", err)
	}
	return mp
}

// corpusConfig builds the seeded city config in Go (config.City uses TOML tags,
// so it is authored here rather than deserialized from the corpus). It mirrors
// the fake-state defaults but names one rig and one agent the assertions expect.
func corpusConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: corpusCityName},
		Agents: []config.Agent{
			{Name: "builder", Dir: corpusRigName, Provider: "test-agent", MaxActiveSessions: intPtr(2)},
		},
		Rigs: []config.Rig{
			{Name: corpusRigName, Path: filepath.Join(os.TempDir(), "dashport-"+corpusRigName)},
		},
		Providers: map[string]config.ProviderSpec{
			"test-agent": {DisplayName: "Test Agent"},
		},
	}
}

// serveSeededCity wires the loaded corpus into the exported production seam.
// The returned stop function drains the plane's run tailers and status samplers.
func serveSeededCity(ctx context.Context, fx *fixtures) (http.Handler, func(), error) {
	return api.ServeSeededCity(ctx, api.SeededCityDeps{
		CityName:      fx.CityName,
		CityPath:      fx.CityPath,
		Config:        fx.config,
		CityBeadStore: fx.cityStore,
		RigStores:     fx.rigStores,
		MailProvider:  fx.mailProv,
		EventProvider: fx.eventProv,
	}, "")
}

func readCorpus(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "dashport", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus %s: %v", path, err)
	}
	return raw
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

func intPtr(n int) *int { return &n }
