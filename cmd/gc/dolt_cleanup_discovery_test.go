package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadRigDoltPorts_ReadsAllRigs(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")
	fs.Files["/rig-a/.beads/dolt-server.port"] = []byte("28232\n")
	fs.Files["/rig-b/.beads/dolt-server.port"] = []byte("28233\n")

	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "alpha", Path: "/rig-a"},
		{Name: "beta", Path: "/rig-b"},
	}

	got := loadRigDoltPorts(rigs, fs)
	want := map[int]string{
		28231: "hq",
		28232: "alpha",
		28233: "beta",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadRigDoltPorts = %v, want %v", got, want)
	}
}

func TestLoadRigDoltPorts_SkipsMissingAndMalformed(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/rig-a/.beads/dolt-server.port"] = []byte("28232\n")
	fs.Files["/rig-b/.beads/dolt-server.port"] = []byte("not-a-port\n")
	fs.Files["/rig-c/.beads/dolt-server.port"] = []byte("\n")
	// /rig-d has no port file at all.

	rigs := []resolverRig{
		{Name: "alpha", Path: "/rig-a"},
		{Name: "beta", Path: "/rig-b"},
		{Name: "gamma", Path: "/rig-c"},
		{Name: "delta", Path: "/rig-d"},
	}

	got := loadRigDoltPorts(rigs, fs)
	want := map[int]string{
		28232: "alpha",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadRigDoltPorts = %v, want %v", got, want)
	}
}

func TestLoadRigDoltPorts_DuplicatePortsLastWins(t *testing.T) {
	// Pathological: two rigs claim the same port. Last write wins so the
	// reaper still protects on port match (it just attributes to the
	// later-listed rig). Acceptable behavior; documented in the function.
	fs := fsys.NewFake()
	fs.Files["/rig-a/.beads/dolt-server.port"] = []byte("28232\n")
	fs.Files["/rig-b/.beads/dolt-server.port"] = []byte("28232\n")

	rigs := []resolverRig{
		{Name: "alpha", Path: "/rig-a"},
		{Name: "beta", Path: "/rig-b"},
	}

	got := loadRigDoltPorts(rigs, fs)
	if got[28232] == "" {
		t.Errorf("expected port 28232 to be in map, got %v", got)
	}
}

func TestSplitCmdline_NULSeparatedWithTrailingNUL(t *testing.T) {
	// /proc/<pid>/cmdline format: NUL-separated argv, trailing NUL.
	in := []byte("dolt\x00sql-server\x00--config\x00/tmp/TestFoo/config.yaml\x00")
	got := splitCmdline(in)
	want := []string{"dolt", "sql-server", "--config", "/tmp/TestFoo/config.yaml"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d, got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitCmdline_Empty(t *testing.T) {
	if got := splitCmdline(nil); len(got) != 0 {
		t.Errorf("splitCmdline(nil) = %v, want empty", got)
	}
	if got := splitCmdline([]byte{}); len(got) != 0 {
		t.Errorf("splitCmdline([]) = %v, want empty", got)
	}
}

func TestParseProcStartTimeTicks(t *testing.T) {
	fieldsAfterComm := []string{
		"S", "1", "2", "3", "4", "5", "6", "7", "8", "9",
		"10", "11", "12", "13", "14", "15", "16", "17", "18", "98765",
	}
	line := "123 (dolt sql server) " + strings.Join(fieldsAfterComm, " ")

	if got := parseProcStartTimeTicks([]byte(line)); got != 98765 {
		t.Fatalf("parseProcStartTimeTicks = %d, want 98765", got)
	}
}

func TestLooksLikeDoltSQLServer(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"absolute dolt path", []string{"/usr/local/bin/dolt", "sql-server"}, true},
		{"bare dolt", []string{"dolt", "sql-server", "--config", "x"}, true},
		{"non-dolt", []string{"mysqld", "sql-server"}, false},
		{"dolt without sql-server", []string{"dolt", "version"}, false},
		{"too short", []string{"dolt"}, false},
		{"empty", []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeDoltSQLServer(tc.argv); got != tc.want {
				t.Errorf("looksLikeDoltSQLServer(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}
