//go:build integration && unix

package integration

import (
	"maps"
	"slices"
	"testing"
)

func TestIntegrationOwnedProcessKillSet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roots []string
		cwd   string
		cmd   string
		self  int
		owned bool
	}{
		{name: "root cwd", roots: []string{"/x/city"}, cwd: "/x/city", owned: true},
		{name: "nested cwd", roots: []string{"/x/city"}, cwd: "/x/city/.beads/dolt", owned: true},
		{name: "rooted argv without cwd", roots: []string{"/x/city"}, cmd: "bd db-proxy-child --config /x/city/.beads/config.yaml", owned: true},
		{name: "equals argv", roots: []string{"/x/city"}, cmd: "dolt sql-server --config=/x/city/.beads/dolt/config.yaml", owned: true},
		{name: "second root", roots: []string{"/x/city", "/x/rig"}, cwd: "/x/rig/work", owned: true},
		{name: "cleaned root", roots: []string{"/x/./city/"}, cwd: "/x/city/work", owned: true},
		{name: "sibling cwd", roots: []string{"/x/city"}, cwd: "/x/city2"},
		{name: "sibling argv", roots: []string{"/x/city"}, cmd: "bd --config /x/city2/.beads/config.yaml"},
		{name: "unrelated", roots: []string{"/x/city"}, cwd: "/elsewhere", cmd: "ordinary worker"},
		{name: "missing cwd and unrelated argv", roots: []string{"/x/city"}, cmd: "bd ready"},
		{name: "self cwd", roots: []string{"/x/city"}, cwd: "/x/city", self: 42},
		{name: "self argv", roots: []string{"/x/city"}, cmd: "bd --config /x/city/.beads/config.yaml", self: 42},
		{name: "empty root", roots: []string{""}, cwd: "/x/city", cmd: "bd /x/city/.beads"},
		{name: "dot root", roots: []string{"."}, cwd: "/x/city", cmd: "bd ./config"},
		{name: "filesystem root", roots: []string{"/"}, cwd: "/x/city", cmd: "bd /x/city/.beads"},
		{name: "no roots", cwd: "/x/city", cmd: "bd /x/city/.beads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procs := map[int]procSnapshot{42: {pid: 42, ppid: 1, cmd: tc.cmd}}
			got := integrationOwnedProcessKillSet(procs, func(int) string { return tc.cwd }, tc.self, tc.roots...)
			want := map[int]bool{}
			if tc.owned {
				want[42] = true
			}
			if !maps.Equal(got, want) {
				t.Fatalf("owned processes = %v, want %v", got, want)
			}
		})
	}
}

func TestStopIntegrationOwnedProcessesReportsSurvivors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		initial   map[int]procSnapshot
		stop      bool
		replace   bool
		want      []string
		wantStops int
	}{
		{
			name:    "already stopped",
			initial: map[int]procSnapshot{},
		},
		{
			name: "only foreign and self remain",
			initial: map[int]procSnapshot{
				99: {pid: 99, ppid: 1, cmd: "test /x/city/test"},
				80: {pid: 80, ppid: 1, cmd: "bd /x/city2/config"},
			},
		},
		{
			name: "successful stop",
			initial: map[int]procSnapshot{
				42: {pid: 42, ppid: 1, cmd: "bd /x/city/config"},
				7:  {pid: 7, ppid: 42, cmd: "dolt /x/rig/config"},
				80: {pid: 80, ppid: 1, cmd: "bd /x/city2/config"},
			},
			stop: true, wantStops: 1,
		},
		{
			name: "client spawns a last child during stop",
			initial: map[int]procSnapshot{
				42: {pid: 42, ppid: 1, cmd: "bd /x/city/config"},
			},
			stop: true, replace: true, wantStops: 2,
		},
		{
			name: "failed stop reports only owned survivors in stable order",
			initial: map[int]procSnapshot{
				42: {pid: 42, ppid: 1, cmd: "bd /x/city/config"},
				7:  {pid: 7, ppid: 42, cmd: "dolt /x/rig/config"},
				80: {pid: 80, ppid: 1, cmd: "bd /x/city2/config"},
				99: {pid: 99, ppid: 1, cmd: "test /x/city/test"},
			},
			want: []string{
				"pid=42 ppid=1 cmd=bd /x/city/config",
				"pid=7 ppid=42 cmd=dolt /x/rig/config",
			},
			wantStops: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := maps.Clone(tc.initial)
			stops := 0
			got := stopIntegrationOwnedProcessesWith(
				func() map[int]procSnapshot { return maps.Clone(live) },
				func(int) string { return "" },
				99,
				func(pids map[int]bool) {
					stops++
					for pid := range pids {
						if pid == 80 || pid == 99 || live[pid].pid != pid {
							t.Fatalf("stop selected foreign, self, or absent process %d", pid)
						}
						if tc.stop {
							delete(live, pid)
						}
					}
					if tc.replace && stops == 1 {
						live[43] = procSnapshot{pid: 43, ppid: 1, cmd: "bd db-proxy-child /x/city/config"}
					}
				},
				"/x/city", "/x/rig",
			)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("survivors = %v, want %v", got, tc.want)
			}
			if stops != tc.wantStops {
				t.Fatalf("stop attempts = %d, want %d", stops, tc.wantStops)
			}
			if _, foreignWasPresent := tc.initial[80]; foreignWasPresent && live[80].pid != 80 {
				t.Fatal("cleanup removed the sibling city's process")
			}
		})
	}
}
