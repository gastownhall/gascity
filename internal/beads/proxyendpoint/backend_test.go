package proxyendpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeRawRecord writes body verbatim as root's proxy.pid.
func writeRawRecord(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(PIDPath(root), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", PIDPath(root), err)
	}
}

// fakeProcessTable is a process table of live PIDs and their argv.
func fakeProcessTable(procs map[int][]string) ProcessTable {
	return ProcessTable{
		Alive: func(pid int) bool { _, ok := procs[pid]; return ok },
		Argv: func(pid int) ([]string, error) {
			argv, ok := procs[pid]
			if !ok {
				return nil, errors.New("no such process")
			}
			return argv, nil
		},
	}
}

func TestConfigOwner(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	proxyArgv := []string{"/usr/local/bin/bd", ChildVerb, "--root", root}
	const recordPID = 4200

	tests := []struct {
		name       string
		record     string // "" writes no proxy.pid
		configPath string
		pt         ProcessTable
		want       bool
	}{
		{name: "config.yaml of a live proxy", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv}), want: true},
		{name: "runtime config of a live proxy", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, RuntimeConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv}), want: true},
		{name: "uncleaned path", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: root + "/./" + ConfigFileName, pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv}), want: true},
		{name: "versioned bd with equals flag", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: {"/opt/beads/bd-1.3.0-rc.2", ChildVerb, "--root=" + root}}), want: true},
		{name: "numeric string pid", record: `{"pid":"4200","kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv}), want: true},
		{name: "retyped and unknown fields", record: `{"pid":4200,"kind":"db-proxy","schema":"two","birth":{"boot":"x"},"port":"none","novel":[1]}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv}), want: true},
		{name: "dead proxy", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(nil)},
		{name: "proxy for another root", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: {"bd", ChildVerb, "--root", other}})},
		{name: "reused pid", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: {"sleep", "60"}})},
		{name: "unreadable argv", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: ProcessTable{Alive: func(int) bool { return true }, Argv: func(int) ([]string, error) { return nil, errors.New("denied") }}},
		{name: "backend kind", record: `{"pid":4200,"kind":"dolt"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv})},
		{name: "no kind", record: `{"pid":4200}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv})},
		{name: "no pid", record: `{"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv})},
		{name: "non-positive pid", record: `{"pid":0,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{0: proxyArgv})},
		{name: "malformed record", record: `not json`, configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv})},
		{name: "no record", configPath: filepath.Join(root, ConfigFileName), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv})},
		{name: "other file name", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, "dolt-config.yaml"), pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv})},
		{name: "empty path", record: `{"pid":4200,"kind":"db-proxy"}`, pt: fakeProcessTable(map[int][]string{recordPID: proxyArgv})},
		{name: "nil Alive", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: ProcessTable{Argv: fakeProcessTable(map[int][]string{recordPID: proxyArgv}).Argv}},
		{name: "nil Argv", record: `{"pid":4200,"kind":"db-proxy"}`, configPath: filepath.Join(root, ConfigFileName), pt: ProcessTable{Alive: func(int) bool { return true }}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(PIDPath(root))
			if tc.record != "" {
				writeRawRecord(t, root, tc.record)
			}
			pid, ok := ConfigOwner(tc.configPath, tc.pt)
			if ok != tc.want {
				t.Fatalf("ConfigOwner(%q) = (%d, %v), want owned=%v", tc.configPath, pid, ok, tc.want)
			}
			if tc.want && pid != recordPID {
				t.Errorf("ConfigOwner(%q) pid = %d, want %d", tc.configPath, pid, recordPID)
			}
			if !tc.want && pid != 0 {
				t.Errorf("ConfigOwner(%q) pid = %d on a refusal, want 0", tc.configPath, pid)
			}
		})
	}
}
