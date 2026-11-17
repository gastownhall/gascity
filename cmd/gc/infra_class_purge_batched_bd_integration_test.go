//go:build integration

package main

import (
	"bytes"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// purgeReservedPorts are ports a test must never bind or reach.
var purgeReservedPorts = map[int]bool{3307: true, 45865: true, 8123: true}

// startPurgeDoltServer starts a throwaway dolt sql-server on a free port
// outside purgeReservedPorts and returns the port. Cleanup stops it by the
// process it started, never by name.
func startPurgeDoltServer(t *testing.T) int {
	t.Helper()
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not found")
	}
	port := 0
	for range 50 {
		candidate := 20000 + rand.IntN(40000)
		if purgeReservedPorts[candidate] {
			continue
		}
		lis, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(candidate))
		if err != nil {
			continue
		}
		if err := lis.Close(); err != nil {
			t.Fatal(err)
		}
		port = candidate
		break
	}
	if port == 0 {
		t.Fatal("no free port outside the reserved ones")
	}
	dataDir := t.TempDir()
	cmd := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", dataDir)
	cmd.Env = append(os.Environ(), "DOLT_ROOT_PATH="+dataDir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting dolt sql-server: %v", err)
	}
	pid := cmd.Process.Pid
	t.Logf("dolt sql-server pid %d on port %d", pid, port)
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // the recorded process, pid
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server pid %d never listened on %d", pid, port)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return port
}

// realBdTierEdgeCity is purgeTierEdgeFixture on a real bd work store served
// by its own dolt sql-server: 4 wisp-tier messages, an issue-tier task, a
// wisp-tier task, and edges of every tier shape. labels names each minted id.
func realBdTierEdgeCity(t *testing.T) (realBdPurgeCity, map[string]string) {
	t.Helper()
	port := startPurgeDoltServer(t)
	city := newRealBdPurgeCity(t, "--server", "--external", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(port), "--server-user", "root")
	ids := map[string]string{}
	for i := 1; i <= 4; i++ {
		ids["msg"+strconv.Itoa(i)] = parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	}
	ids["work"] = parseCreatedBeadID(t, city.must("bd", "create", "--json", "ordinary backlog work", "-t", "task"))
	ids["wisp"] = parseCreatedBeadID(t, city.must("bd", "create", "--json", "a wisp-tier step", "-t", "task", "--ephemeral"))
	for _, edge := range [][2]string{
		{"work", "msg1"}, // issue tier into a message
		{"msg2", "work"}, // a message out to the issue tier
		{"wisp", "msg3"}, // a non-selected wisp row into a message
		{"msg1", "msg4"}, // message to message
		{"msg4", "wisp"}, // a message out to the wisp row
		{"work", "wisp"}, // untouched by the purge
	} {
		city.must("bd", "dep", "add", ids[edge[0]], ids[edge[1]])
	}
	labels := map[string]string{}
	for label, id := range ids {
		labels[id] = label
	}
	return city, labels
}

// TestPurgeStrandedBatchedEdgeReadMatchesThePerRowReadOnRealBd is
// TestPurgeStrandedBatchedEdgeReadMatchesThePerRowRead on real bd over a dolt
// sql-server: the batched run (BdStore.DepListBatch, which T6a lets the
// purge's WorkStore handle reach) leaves exactly what the per-row run leaves.
func TestPurgeStrandedBatchedEdgeReadMatchesThePerRowReadOnRealBd(t *testing.T) {
	run := func(wrap func(*beads.BdStore, *purgeBatchProbe) beads.Store) (purgeOutcome, *purgeBatchProbe, map[string]string) {
		city, labels := realBdTierEdgeCity(t)
		probe := &purgeBatchProbe{}
		openInfraMigrationSource = func(string) (beads.Store, error) {
			return wrapStoreWithBeadPolicies(wrap(city.store(), probe), city.cfg), nil
		}
		var stdout, stderr bytes.Buffer
		if code := runStrandedPurge(t, city.purgeCity, confirmedPurge(t, city.purgeCity, 4), &stdout, &stderr); code != 0 {
			t.Fatalf("purge exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
		}
		label := func(id string) string {
			if l, ok := labels[id]; ok {
				return l
			}
			return id
		}
		return readPurgeOutcomeFrom(t, city.purgeCity, city.store(), label), probe, labels
	}
	batched, batchProbe, labels := run(func(s *beads.BdStore, p *purgeBatchProbe) beads.Store { return bdBatchingStore{s, p} })
	perRow, perRowProbe, _ := run(func(s *beads.BdStore, p *purgeBatchProbe) beads.Store { return bdPerRowStore{s, p} })

	selected := map[string]bool{}
	for id, label := range labels {
		if strings.HasPrefix(label, "msg") {
			selected[id] = true
		}
	}
	if !batchProbe.readAnchorOutside(selected) {
		t.Errorf("the batched run never read the inbound edges in batches (batches %v)", batchProbe.batches)
	}
	if n := batchProbe.upReads(); n != 0 {
		t.Errorf("the batched run made %d per-row dependents read(s): bd's batch did not answer", n)
	}
	if len(perRowProbe.batches) == 0 {
		t.Error("the per-row run never asked for a batch; it does not prove the fallback")
	}
	if want := []string{"wisp", "work"}; !slices.Equal(batched.rows, want) {
		t.Errorf("rows left = %v, want %v", batched.rows, want)
	}
	if want := []string{"work -[blocks]-> wisp"}; !slices.Equal(batched.edges, want) {
		t.Errorf("edges left = %v, want %v", batched.edges, want)
	}
	for id, want := range map[string]int{"msg1": 1, "msg3": 1, "msg4": 1} {
		if got := batched.inbound[id]; got != want {
			t.Errorf("the backup of %s holds %d inbound edge(s), want %d", id, got, want)
		}
	}
	if !slices.Equal(batched.rows, perRow.rows) || !slices.Equal(batched.edges, perRow.edges) {
		t.Errorf("work store: batched rows %v edges %v, per-row rows %v edges %v", batched.rows, batched.edges, perRow.rows, perRow.edges)
	}
	if !slices.Equal(batched.backup, perRow.backup) {
		t.Errorf("backup: batched\n  %v\nper-row\n  %v", batched.backup, perRow.backup)
	}
}

// bdBatchingStore is a bd work store that answers batched edge reads and
// records them.
type bdBatchingStore struct {
	*beads.BdStore
	probe *purgeBatchProbe
}

func (s bdBatchingStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.probe.record(ids)
	return s.BdStore.DepListBatch(ids)
}

func (s bdBatchingStore) DepList(id, direction string) ([]beads.Dep, error) {
	if direction == "up" {
		s.probe.recordUp()
	}
	return s.BdStore.DepList(id, direction)
}

// bdPerRowStore is a bd work store whose batched edge read reports itself
// unsupported, so the purge reads every row's edges on its own.
type bdPerRowStore struct {
	*beads.BdStore
	probe *purgeBatchProbe
}

func (s bdPerRowStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) { //nolint:unparam // implements beads.DependencyBatchLister; its whole answer is the refusal
	s.probe.record(ids)
	return nil, beads.ErrDepListBatchUnsupported
}
