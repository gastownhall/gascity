//go:build integration

package beads_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestBdStoreConcurrentDifferentKeySetMetadataComposesOnSharedServer is the
// regression row for gascity#1596: two processes running
// `bd update <id> --set-metadata` on different keys of one bead must both
// land. bd <= v1.2.2 merged each key into a client-side snapshot and wrote the
// whole metadata document back, so one writer silently erased the other's key
// while both exited 0; bd >= v1.3.0 resolves the key against the row it
// re-reads inside the write transaction.
//
// The scope is a server-mode scope on a private dolt sql-server, created by
// the bd under test. An embedded scope serializes writers on the embedded
// database, so even a broken bd never loses a key there and the row would
// prove nothing.
func TestBdStoreConcurrentDifferentKeySetMetadataComposesOnSharedServer(t *testing.T) {
	port := beads.StartTestDoltServerForTest(t)
	dir := newIntegrationGitScopeDir(t)
	// bd's test-mode guard resolves a server-bound scope to the 127.0.0.1:1
	// sentinel (beadstest.EnvBeadsTestMode), so this scope runs with it off.
	runner := newIntegrationBdRunner(t, dir, map[string]string{beadstest.EnvBeadsTestMode: "0"})
	if out, err := runner(dir, "bd", "init", "-p", "tst",
		"--server", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(port),
		"--server-user", "root", "--external", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("bd init --server: %v\n%s", err, out)
	}
	requireServerModeScope(t, dir)
	version, err := runner(dir, "bd", "version")
	if err != nil {
		t.Fatalf("bd version: %v\n%s", err, version)
	}
	t.Logf("bd under test: %s", strings.TrimSpace(string(version)))

	stores := []*beads.BdStore{beads.NewBdStore(dir, runner), beads.NewBdStore(dir, runner)}
	created, err := stores[0].Create(beads.Bead{Title: "compose"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.ID

	const rounds = 50
	var lost []string
	for round := 1; round <= rounds; round++ {
		value := fmt.Sprintf("v%d", round)
		raceBdStoreWrites(t, fmt.Sprintf("round %d", round),
			func() error { return stores[0].SetMetadata(id, "k1", value) },
			func() error { return stores[1].SetMetadata(id, "k2", value) },
		)
		got := getBdBead(t, stores[0], id)
		if got.Metadata["k1"] != value || got.Metadata["k2"] != value {
			lost = append(lost, fmt.Sprintf("round %d: k1=%q k2=%q (want %q)",
				round, got.Metadata["k1"], got.Metadata["k2"], value))
		}
	}
	if len(lost) > 0 {
		t.Fatalf("%d of %d rounds lost a key:\n%s", len(lost), rounds, strings.Join(lost, "\n"))
	}

	// Same key: both writes succeed and the last writer wins.
	raceBdStoreWrites(t, "same-key round",
		func() error { return stores[0].SetMetadata(id, "k1", "same-a") },
		func() error { return stores[1].SetMetadata(id, "k1", "same-b") },
	)
	got := getBdBead(t, stores[0], id)
	if v := got.Metadata["k1"]; v != "same-a" && v != "same-b" {
		t.Fatalf("same-key round: k1=%q, want one of the two writes", v)
	}

	// A three-key batch racing a single-key writer: every key lands.
	batch := map[string]string{"b1": "x", "b2": "y", "b3": "z"}
	raceBdStoreWrites(t, "batch round",
		func() error { return stores[0].SetMetadataBatch(id, batch) },
		func() error { return stores[1].SetMetadata(id, "k2", "solo") },
	)
	got = getBdBead(t, stores[0], id)
	for k, want := range batch {
		if got.Metadata[k] != want {
			t.Errorf("batch round: %s=%q, want %q (metadata %v)", k, got.Metadata[k], want, got.Metadata)
		}
	}
	if got.Metadata["k2"] != "solo" {
		t.Errorf("batch round: k2=%q, want %q (metadata %v)", got.Metadata["k2"], "solo", got.Metadata)
	}
}

// requireServerModeScope fails the test unless dir's bd scope is in server
// mode: a scope that fell back to embedded mode cannot lose a key.
func requireServerModeScope(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, ".beads", "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scope metadata: %v", err)
	}
	var meta struct {
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	if meta.DoltMode != "server" {
		t.Fatalf("%s dolt_mode = %q, want %q:\n%s", path, meta.DoltMode, "server", data)
	}
}

// raceBdStoreWrites releases both writes from one barrier and fails the test
// if either returns an error: a writer that lost its write must say so.
func raceBdStoreWrites(t *testing.T, label string, writes ...func() error) {
	t.Helper()
	errs := make([]error, len(writes))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, write := range writes {
		wg.Add(1)
		go func(i int, write func() error) {
			defer wg.Done()
			<-start
			errs[i] = write()
		}(i, write)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("%s: writer %d: %v", label, i, err)
		}
	}
}

func getBdBead(t *testing.T, store *beads.BdStore, id string) beads.Bead {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return got
}
