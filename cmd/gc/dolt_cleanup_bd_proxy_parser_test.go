package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// TestBdOwnedProxyDoltConfigDecisionTable states the reaper's ownership proof
// as one table, so moving its parser into internal/beads/proxyendpoint is a
// refactor with a fence around it rather than a diff nobody can check.
//
// Every row is a record plus a process table plus the one answer the reaper
// must give. The rows that matter most are the last two: the reaper's question
// is narrower than the endpoint reader's, and a record that could never be
// dialed must still protect the process it names.
func TestBdOwnedProxyDoltConfigDecisionTable(t *testing.T) {
	const pid = 7701

	cases := []struct {
		name string
		// record is the proxy.pid body, or empty for no record at all.
		record string
		// argv is the fabricated process table entry for pid, or nil for a
		// process table that has no such process.
		argv func(root string) []string
		want bool
	}{
		{
			name:   "bd's canonical live proxy",
			record: `{"pid":7701,"port":35425,"upstream_id":"u","schema":2,"kind":"db-proxy","control_port":46445}`,
			argv:   func(root string) []string { return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", root} },
			want:   true,
		},
		{
			name: "no record at all",
			argv: func(root string) []string { return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", root} },
		},
		{
			name:   "a record that is not JSON",
			record: "not json",
			argv:   func(root string) []string { return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", root} },
		},
		{
			name:   "the dolt-backend record, not the proxy's",
			record: `{"pid":7701,"schema":2,"kind":"dolt-backend"}`,
			argv:   func(root string) []string { return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", root} },
		},
		{
			name:   "pid 0",
			record: `{"pid":0,"schema":2,"kind":"db-proxy"}`,
			argv:   func(root string) []string { return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", root} },
		},
		{
			name:   "a dead pid",
			record: `{"pid":7701,"schema":2,"kind":"db-proxy"}`,
		},
		{
			name:   "a recycled pid running something else",
			record: `{"pid":7701,"schema":2,"kind":"db-proxy"}`,
			argv:   func(string) []string { return []string{"/usr/bin/sleep", "infinity"} },
		},
		{
			name:   "bd, but not the proxy verb",
			record: `{"pid":7701,"schema":2,"kind":"db-proxy"}`,
			argv:   func(string) []string { return []string{"/usr/local/bin/bd", "list", "--json"} },
		},
		{
			name:   "the proxy verb, rooted elsewhere",
			record: `{"pid":7701,"schema":2,"kind":"db-proxy"}`,
			argv: func(string) []string {
				return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", "/somewhere/else"}
			},
		},
		{
			name:   "a versioned bd binary is still bd",
			record: `{"pid":7701,"schema":2,"kind":"db-proxy"}`,
			argv: func(root string) []string {
				return []string{"/opt/beads/bd-1.3.0-rc.2", "db-proxy-child", "--root", root}
			},
			want: true,
		},
		{
			name:   "the joined --root spelling",
			record: `{"pid":7701,"schema":2,"kind":"db-proxy"}`,
			argv:   func(root string) []string { return []string{"/usr/local/bin/bd", "db-proxy-child", "--root=" + root} },
			want:   true,
		},
		{
			// A record from before schema 2, carrying neither a birth token nor
			// a root identity. proxyendpoint.Validate refuses it — gc cannot
			// establish which generation it describes, so gc must not dial it —
			// but the process it names is still bd's, and reaping it would take
			// bd's proxy down.
			name:   "a legacy record still protects its process",
			record: `{"pid":7701,"port":35425,"kind":"db-proxy"}`,
			argv:   func(root string) []string { return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", root} },
			want:   true,
		},
		{
			// The same point from the other side: a record naming another
			// workspace's root_id. The endpoint reader refuses it as foreign,
			// and the reaper still protects, because the argv — not the record's
			// identity fields — is what proves whose process this is.
			name:   "a foreign root_id still protects its process",
			record: `{"pid":7701,"port":35425,"schema":2,"kind":"db-proxy","birth":"linux-v1:boot:1","root_id":"deadbeef"}`,
			argv:   func(root string) []string { return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", root} },
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := t.TempDir()
			root := filepath.Join(scope, ".beads", "dolt")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(configPath, []byte("listener:\n  host: 127.0.0.1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.record != "" {
				if err := os.WriteFile(filepath.Join(root, "proxy.pid"), []byte(tc.record), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			procs := map[int][]string{}
			if tc.argv != nil {
				procs[pid] = tc.argv(root)
			}
			stubBdProxyProcesses(t, procs)

			gotPID, got := bdOwnedProxyDoltConfig(configPath)
			if got != tc.want {
				t.Fatalf("bdOwnedProxyDoltConfig = %v, want %v", got, tc.want)
			}
			if got && gotPID != pid {
				t.Fatalf("bdOwnedProxyDoltConfig returned pid %d, want %d", gotPID, pid)
			}

			// The reaper's own front door must agree: protection is what the
			// predicate is for, and a predicate that answered correctly while
			// the classifier reaped anyway would be worthless.
			p := DoltProcInfo{PID: 9100, Argv: []string{"dolt", "sql-server", "--config", configPath}}
			classified := classifyDoltProcess(p, nil, "/home/u", os.TempDir(), nil)
			wantAction := "reap"
			if tc.want {
				wantAction = "protect"
			}
			if classified.Action != wantAction {
				t.Fatalf("classifyDoltProcess action = %q, want %q; reason = %q", classified.Action, wantAction, classified.Reason)
			}
		})
	}
}

// TestReaperProofIsNarrowerThanEndpointAdmission states the boundary between
// the two questions in one place, so a later slice that widens the reaper onto
// proxyendpoint.Validate has to delete this test on purpose.
//
// Killing bd's proxy is irreversible and dialing it is not, so the two
// questions fail in opposite directions: admission refuses whatever it cannot
// prove, and protection protects whatever it cannot rule out.
func TestReaperProofIsNarrowerThanEndpointAdmission(t *testing.T) {
	scope := t.TempDir()
	root := filepath.Join(scope, ".beads", "dolt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("listener:\n  host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const pid = 7702
	// Schema 1: bd's own ValidateV2 rejects it, and so does gc's admission.
	record := fmt.Sprintf(`{"pid":%d,"port":35425,"schema":1,"kind":"db-proxy"}`, pid)
	if err := os.WriteFile(filepath.Join(root, "proxy.pid"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	stubBdProxyProcesses(t, map[int][]string{
		pid: {"/usr/local/bin/bd", "db-proxy-child", "--root", root},
	})

	ep := proxyendpoint.Inspect(root, proxyendpoint.DefaultProcessTable())
	if ep.Verdict.Live() {
		t.Fatalf("admission accepted a schema-1 record (%v); this test's premise is gone", ep.Verdict)
	}
	if _, ok := bdOwnedProxyDoltConfig(configPath); !ok {
		t.Fatal("the reaper stopped protecting a live bd proxy whose record admission refuses; an unreapable-process guard must not inherit a dial gate's strictness")
	}
}
