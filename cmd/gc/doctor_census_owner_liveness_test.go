package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func writeCensusLedger(t *testing.T, scopePath, toml string) {
	t.Helper()
	dir := filepath.Join(scopePath, "test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir test dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test-resources.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

func TestCensusOwnerLivenessCheckSkipsScopeWithoutLedgerFile(t *testing.T) {
	cityDir := t.TempDir()
	result := newCensusOwnerLivenessCheck(nil, cityDir, func(string) (beads.Store, error) {
		t.Fatal("newStore should not be called when no ledger file exists")
		return nil, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if len(result.Details) != 0 {
		t.Fatalf("details = %v, want empty", result.Details)
	}
}

func TestCensusOwnerLivenessCheckOKWhenAllOwnerBeadsAlive(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-alive-1"

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "ga-alive-2"
`)
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "ga-alive-1", Title: "alive one"},
		{ID: "ga-alive-2", Title: "alive two"},
	}, nil)
	result := newCensusOwnerLivenessCheck(nil, cityDir, func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK; message=%q details=%v", result.Status, result.Message, result.Details)
	}
}

func TestCensusOwnerLivenessCheckWarnsOnDanglingOwnerBead(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-missing-1"
`)
	store := beads.NewMemStoreFrom(0, nil, nil)
	result := newCensusOwnerLivenessCheck(nil, cityDir, func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "dangling owner_bead=ga-missing-1") {
		t.Fatalf("details missing dangling owner_bead marker:\n%s", details)
	}
	if !strings.Contains(result.FixHint, "council review") {
		t.Fatalf("fix hint = %q, want mention of council review", result.FixHint)
	}
}

func TestCensusOwnerLivenessCheckDedupesRepeatedOwnerBeadAcrossRows(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-missing-shared"

[[debt]]
scope = "untagged"
resource = "subprocess"
owner_bead = "ga-missing-shared"

[[medium]]
package_dir = "cmd/gc"
package_name = "main"
owner = "TestFoo"
resources = ["subprocess"]
owner_bead = "ga-missing-shared"

[[small_debt]]
scope = "all"
resource = "fixed_sleep"
owner_bead = "ga-missing-shared"
`)
	inner := beads.NewMemStoreFrom(0, nil, nil)
	spy := &censusGetCountingStore{Store: inner, counts: map[string]int{}}
	result := newCensusOwnerLivenessCheck(nil, cityDir, func(string) (beads.Store, error) {
		return spy, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if got := spy.counts["ga-missing-shared"]; got != 1 {
		t.Fatalf("Get(ga-missing-shared) called %d times, want 1 (dedup across rows)", got)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"audit_baseline:", "debt:", "medium:", "small_debt:"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing row category %q:\n%s", want, details)
		}
	}
	danglingCount := strings.Count(details, "dangling owner_bead=ga-missing-shared")
	if danglingCount != 1 {
		t.Fatalf("dangling owner_bead=ga-missing-shared appeared %d times, want exactly 1 finding line:\n%s", danglingCount, details)
	}
}

func TestCensusOwnerLivenessCheckSkipsOnStoreOpenFailure(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-whatever"
`)
	result := newCensusOwnerLivenessCheck(nil, cityDir, func(string) (beads.Store, error) {
		return nil, errors.New("city offline")
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "skipped: opening bead store: city offline") {
		t.Fatalf("details missing store-open skip marker:\n%s", details)
	}
	if strings.Contains(details, "dangling") {
		t.Fatalf("store-open failure must not be reported as dangling:\n%s", details)
	}
	if result.FixHint != "fix bead store access, then rerun gc doctor" {
		t.Fatalf("fix hint = %q, want store-access hint", result.FixHint)
	}
}

func TestCensusOwnerLivenessCheckSkipsOnNonNotFoundGetError(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-transient"
`)
	store := censusGetErrorStore{err: errors.New("connection reset")}
	result := newCensusOwnerLivenessCheck(nil, cityDir, func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "skipped: checking owner_bead ga-transient: connection reset") {
		t.Fatalf("details missing get-error skip marker:\n%s", details)
	}
	if strings.Contains(details, "dangling") {
		t.Fatalf("non-not-found Get error must not be reported as dangling:\n%s", details)
	}
}

func TestCensusOwnerLivenessCheckScansCityAndRigs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-city-missing"
`)
	writeCensusLedger(t, rigDir, `
version = 1

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "ga-rig-missing"
`)
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "repo", Path: rigDir},
			{Name: "ghost", Path: ""},
		},
	}
	store := beads.NewMemStoreFrom(0, nil, nil)
	result := newCensusOwnerLivenessCheck(cfg, cityDir, func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"city: dangling owner_bead=ga-city-missing",
		"rig repo: dangling owner_bead=ga-rig-missing",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

type censusGetCountingStore struct {
	beads.Store
	counts map[string]int
}

func (s *censusGetCountingStore) Get(id string) (beads.Bead, error) {
	s.counts[id]++
	return s.Store.Get(id)
}

type censusGetErrorStore struct {
	beads.Store
	err error
}

func (s censusGetErrorStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, s.err
}
