package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCleanupReportJSONShape(t *testing.T) {
	r := CleanupReport{
		Schema: "gc.dolt.cleanup.v1",
		Port: CleanupPortReport{
			Resolved: 28231,
			Source:   "/city/.beads/dolt-server.port",
			Fallback: false,
		},
		Dropped: CleanupDroppedReport{
			Skipped: []DoltDropSkip{{
				Name:   "testdb.invalid",
				Reason: DropSkipReasonInvalidIdentifier,
			}},
		},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)

	wantKeys := []string{
		`"schema":"gc.dolt.cleanup.v1"`,
		`"port":{`,
		`"rigs_protected":[]`,
		`"dropped":{`,
		`"purge":{`,
		`"reaped":{`,
		`"summary":{`,
		`"errors":[]`,
		`"skipped":[{"name":"testdb.invalid","reason":"invalid-identifier"}]`,
	}
	for _, key := range wantKeys {
		if !strings.Contains(got, key) {
			t.Errorf("JSON missing %q\nfull JSON:\n%s", key, got)
		}
	}
	for _, key := range []string{`"Name":"testdb.invalid"`, `"Reason":"invalid-identifier"`} {
		if strings.Contains(got, key) {
			t.Errorf("JSON leaked Go field name %q\nfull JSON:\n%s", key, got)
		}
	}
}

func TestRunDoltCleanup_JSONOutputsResolvedPort(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Flag:     "",
		CityPort: 0,
		Rigs:     rigs,
		FS:       fs,
		JSON:     true,
		Probe:    false, // skip TCP probe in unit tests
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runDoltCleanup exit=%d, stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal stdout: %v\nstdout: %s", err, stdout.String())
	}
	if r.Schema != "gc.dolt.cleanup.v1" {
		t.Errorf("Schema = %q", r.Schema)
	}
	if r.Port.Resolved != 28231 {
		t.Errorf("Port.Resolved = %d, want 28231", r.Port.Resolved)
	}
	if r.Port.Fallback {
		t.Errorf("Port.Fallback = true, want false")
	}
}

func TestRunDoltCleanup_HumanOutputShowsPortAndFallbackWarning(t *testing.T) {
	fs := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:    fs,
		JSON:  false,
		Probe: false,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "3307") {
		t.Errorf("stdout missing legacy port 3307: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "fallback") && !strings.Contains(strings.ToLower(out), "legacy default") {
		t.Errorf("stdout missing fallback indicator: %s", out)
	}
}

func TestRunDoltCleanup_FlagOverridesEverything(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Flag:     "9999",
		CityPort: 4242,
		Rigs:     []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:       fs,
		JSON:     true,
		Probe:    false,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Port.Resolved != 9999 {
		t.Errorf("Port.Resolved = %d, want 9999", r.Port.Resolved)
	}
	if r.Port.Source != "--port flag" {
		t.Errorf("Port.Source = %q", r.Port.Source)
	}
}

func TestRunDoltCleanup_ForceProtectsSelectedPortWithoutRigPortFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flag     string
		cityPort int
		wantPort int
	}{
		{name: "flag", flag: "43306", cityPort: 43307, wantPort: 43306},
		{name: "city config", cityPort: 43307, wantPort: 43307},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var killed []syscall.Signal

			var stdout, stderr bytes.Buffer
			opts := cleanupOptions{
				Flag:     tc.flag,
				CityPort: tc.cityPort,
				Rigs:     []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
				FS:       fsys.NewFake(),
				JSON:     true,
				Force:    true,
				HomeDir:  "/home/u",
				DiscoverProcesses: func() ([]DoltProcInfo, error) {
					return []DoltProcInfo{{
						PID:            4444,
						Ports:          []int{tc.wantPort},
						Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestActive/config.yaml"},
						StartTimeTicks: 10,
					}}, nil
				},
				KillProcess: func(_ int, sig syscall.Signal) error {
					killed = append(killed, sig)
					return nil
				},
				ReapGracePeriod: 1,
			}
			code := runDoltCleanup(opts, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
			}
			var r CleanupReport
			if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
				t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
			}

			if r.Port.Resolved != tc.wantPort {
				t.Fatalf("Port.Resolved = %d, want %d", r.Port.Resolved, tc.wantPort)
			}
			if len(killed) != 0 {
				t.Fatalf("KillProcess called for selected active port: %v", killed)
			}
			if r.Reaped.Count != 0 {
				t.Errorf("Reaped.Count = %d, want 0 for process listening on selected port", r.Reaped.Count)
			}
			if !equalIntSlice(r.Reaped.ProtectedPIDs, []int{4444}) {
				t.Errorf("ProtectedPIDs = %v, want [4444]", r.Reaped.ProtectedPIDs)
			}
		})
	}
}

func TestRunDoltCleanup_InvalidPortFlagIsFatal(t *testing.T) {
	for _, flag := range []string{"not-a-number", "0", "-1"} {
		t.Run(flag, func(t *testing.T) {
			client := &fakeCleanupDoltClient{
				databases: []string{"testdb_abc"},
			}
			var killed []syscall.Signal

			var stdout, stderr bytes.Buffer
			opts := cleanupOptions{
				Flag:       flag,
				CityPort:   4242,
				FS:         fsys.NewFake(),
				JSON:       true,
				Force:      true,
				DoltClient: client,
				DiscoverProcesses: func() ([]DoltProcInfo, error) {
					return []DoltProcInfo{{PID: 4444, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"}}}, nil
				},
				KillProcess: func(_ int, sig syscall.Signal) error {
					killed = append(killed, sig)
					return nil
				},
				ReapGracePeriod: 1,
			}
			code := runDoltCleanup(opts, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("exit=0, want invalid explicit --port to fail\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
			}

			var r CleanupReport
			if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
				t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
			}
			if len(client.dropped) != 0 {
				t.Fatalf("DropDatabase called for invalid --port: %v", client.dropped)
			}
			if len(killed) != 0 {
				t.Fatalf("KillProcess called for invalid --port: %v", killed)
			}
			foundPortError := false
			for _, entry := range r.Errors {
				if entry.Stage == "port" && strings.Contains(entry.Error, "invalid port") {
					foundPortError = true
				}
			}
			if !foundPortError {
				t.Fatalf("Errors missing fatal port validation entry: %+v", r.Errors)
			}
		})
	}
}

func TestRunDoltCleanup_BadRigPortFileIsFatal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*fsys.Fake)
		wantError string
	}{
		{
			name:      "empty",
			setup:     func(fs *fsys.Fake) { fs.Files["/city/.beads/dolt-server.port"] = []byte("\n") },
			wantError: "empty",
		},
		{
			name:      "malformed",
			setup:     func(fs *fsys.Fake) { fs.Files["/city/.beads/dolt-server.port"] = []byte("not-a-port\n") },
			wantError: "invalid port",
		},
		{
			name:      "unreadable",
			setup:     func(fs *fsys.Fake) { fs.Errors["/city/.beads/dolt-server.port"] = os.ErrPermission },
			wantError: "permission",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			tc.setup(fs)
			client := &fakeCleanupDoltClient{
				databases: []string{"testdb_abc"},
			}
			var killed []syscall.Signal

			var stdout, stderr bytes.Buffer
			opts := cleanupOptions{
				Rigs:       []resolverRig{{Name: "city", Path: "/city", HQ: true}},
				FS:         fs,
				JSON:       true,
				Force:      true,
				DoltClient: client,
				DiscoverProcesses: func() ([]DoltProcInfo, error) {
					return []DoltProcInfo{{PID: 4444, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"}}}, nil
				},
				KillProcess: func(_ int, sig syscall.Signal) error {
					killed = append(killed, sig)
					return nil
				},
				ReapGracePeriod: 1,
			}
			code := runDoltCleanup(opts, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("exit=0, want bad rig port file to fail closed\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
			}

			var r CleanupReport
			if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
				t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
			}
			if len(client.dropped) != 0 {
				t.Fatalf("DropDatabase called after bad rig port file: %v", client.dropped)
			}
			if len(killed) != 0 {
				t.Fatalf("KillProcess called after bad rig port file: %v", killed)
			}
			if r.Port.Resolved != 0 {
				t.Fatalf("Port.Resolved = %d, want 0 for unresolved fatal port", r.Port.Resolved)
			}
			foundPortError := false
			for _, entry := range r.Errors {
				if entry.Stage == "port" && strings.Contains(entry.Error, tc.wantError) {
					foundPortError = true
				}
			}
			if !foundPortError {
				t.Fatalf("Errors missing fatal rig port-file entry containing %q: %+v", tc.wantError, r.Errors)
			}
		})
	}
}

func TestRunDoltCleanup_SQLClientOpenFailureIsTypedAndFatal(t *testing.T) {
	fs := fsys.NewFake()
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"dropped_db/data.bin": 4096,
	})
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "city", Path: "/city", HQ: true}},
		FS:                fs,
		JSON:              true,
		Force:             true,
		DoltClientOpenErr: fmt.Errorf("open dolt connection: refused"),
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit=0, want SQL open failure to make forced cleanup fail\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if r.Summary.ErrorsTotal != 2 {
		t.Fatalf("Summary.ErrorsTotal = %d, want drop and purge open errors; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
	hasDrop := false
	hasPurge := false
	for _, entry := range r.Errors {
		if strings.Contains(entry.Error, "open dolt connection: refused") {
			switch entry.Stage {
			case "drop":
				hasDrop = true
			case "purge":
				hasPurge = true
			}
		}
	}
	if !hasDrop || !hasPurge {
		t.Fatalf("Errors = %+v, want typed drop and purge SQL-open errors", r.Errors)
	}
	if r.Purge.OK {
		t.Fatalf("Purge.OK = true, want false when SQL-backed purge could not run")
	}
	if r.Summary.BytesFreedDisk != 0 {
		t.Fatalf("Summary.BytesFreedDisk = %d, want 0 because forced purge did not run", r.Summary.BytesFreedDisk)
	}
}

func TestRunDoltCleanup_RigsProtectedFromRegistry(t *testing.T) {
	// Wireframe-6 schema requires rigs_protected to enumerate registered rigs.
	// One entry per registered rig (HQ + non-HQ); each rig's DB name equals
	// its rig name in this codebase (`gascity`, `beads`, etc.). Order is
	// HQ-first to match the resolver's port-resolution preference.
	fs := fsys.NewFake()
	rigs := []resolverRig{
		{Name: "gascity", Path: "/city", HQ: true},
		{Name: "beads", Path: "/beads", HQ: false},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:  rigs,
		FS:    fs,
		JSON:  true,
		Probe: false,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := []CleanupRigProtection{
		{Rig: "gascity", DB: "gascity"},
		{Rig: "beads", DB: "beads"},
	}
	if len(r.RigsProtected) != len(want) {
		t.Fatalf("RigsProtected len = %d, want %d (got %v)", len(r.RigsProtected), len(want), r.RigsProtected)
	}
	for i, w := range want {
		if r.RigsProtected[i] != w {
			t.Errorf("RigsProtected[%d] = %+v, want %+v", i, r.RigsProtected[i], w)
		}
	}
}

func TestRunDoltCleanup_DryRunReportsReapPlanWithoutKilling(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	procs := []DoltProcInfo{
		{PID: 1138290, Ports: []int{28231}, Argv: []string{"dolt", "sql-server"}},
		{PID: 1281044, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestA/config.yaml"}},
		{PID: 1319499, Ports: []int{33400}, Argv: []string{"dolt", "sql-server", "--config", "/tmp/be-s9d-bench-dolt/config.yaml"}},
	}
	killed := []int{}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:    []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:      fs,
		JSON:    true,
		Probe:   false,
		HomeDir: "/home/u",
		// Force not set → dry-run.
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess: func(pid int, _ syscall.Signal) error {
			killed = append(killed, pid)
			return nil
		},
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	if r.Reaped.Count != 1 {
		t.Errorf("Reaped.Count = %d, want 1 (one orphan, dry-run)", r.Reaped.Count)
	}
	wantProtected := []int{1138290, 1319499}
	if !equalIntSlice(r.Reaped.ProtectedPIDs, wantProtected) {
		t.Errorf("ProtectedPIDs = %v, want %v", r.Reaped.ProtectedPIDs, wantProtected)
	}
	if len(killed) != 0 {
		t.Errorf("KillProcess called %d times in dry-run; want 0 (dry-run is non-destructive)", len(killed))
	}
}

func TestRunDoltCleanup_DryRunAllowsProcessTempRootTestConfig(t *testing.T) {
	procs := []DoltProcInfo{{
		PID:  1281044,
		Argv: []string{"dolt", "sql-server", "--config", "/var/tmp/go-test/TestA/config.yaml"},
	}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		TempDir:           "/var/tmp/go-test",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	if r.Reaped.Count != 1 {
		t.Errorf("Reaped.Count = %d, want 1 for os.TempDir()/Test* config", r.Reaped.Count)
	}
	if len(r.Reaped.ProtectedPIDs) != 0 {
		t.Errorf("ProtectedPIDs = %v, want none for os.TempDir()/Test* config", r.Reaped.ProtectedPIDs)
	}
}

func TestRunDoltCleanup_ForceKillsOrphans(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	procs := []DoltProcInfo{
		{PID: 1138290, Ports: []int{28231}, Argv: []string{"dolt", "sql-server"}, StartTimeTicks: 10},
		{PID: 1281044, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestA/config.yaml"}, StartTimeTicks: 20},
		{PID: 1281099, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestB/config.yaml"}, StartTimeTicks: 30},
	}
	var termed []int

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:                fs,
		JSON:              true,
		Force:             true,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess: func(pid int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				termed = append(termed, pid)
			}
			return syscall.ESRCH // pretend the process is already gone after TERM
		},
		ReapGracePeriod: 1, // tiny so the test doesn't sleep meaningfully
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Reaped.Count != 2 {
		t.Errorf("Reaped.Count = %d, want 2", r.Reaped.Count)
	}
	wantTermed := []int{1281044, 1281099}
	if !equalIntSlice(termed, wantTermed) {
		t.Errorf("SIGTERM-ed PIDs = %v, want %v", termed, wantTermed)
	}
}

func TestRunDoltCleanup_ForceReportsReapedRSSBytes(t *testing.T) {
	procs := []DoltProcInfo{
		{PID: 1281044, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestA/config.yaml"}, RSSBytes: 4096, StartTimeTicks: 20},
		{PID: 1281099, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestB/config.yaml"}, RSSBytes: 8192, StartTimeTicks: 30},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		Force:             true,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess:       func(_ int, _ syscall.Signal) error { return syscall.ESRCH },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	if r.Reaped.Count != 2 {
		t.Fatalf("Reaped.Count = %d, want 2", r.Reaped.Count)
	}
	if r.Summary.BytesFreedRSS != 12288 {
		t.Errorf("Summary.BytesFreedRSS = %d, want 12288", r.Summary.BytesFreedRSS)
	}
}

func TestRunDoltCleanup_ForceCountsSuccessfulKill(t *testing.T) {
	procs := []DoltProcInfo{
		{PID: 4444, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"}, StartTimeTicks: 10},
	}
	var signals []syscall.Signal

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		Force:             true,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Reaped.Count != len(procs) {
		t.Errorf("Reaped.Count = %d, want %d", r.Reaped.Count, len(procs))
	}
	if r.Summary.ErrorsTotal != 0 {
		t.Errorf("Summary.ErrorsTotal = %d, want 0", r.Summary.ErrorsTotal)
	}
	if len(r.Errors) != 0 || len(r.Reaped.Errors) != 0 {
		t.Errorf("errors = %#v, reap errors = %#v; want none", r.Errors, r.Reaped.Errors)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Errorf("signals = %v, want [SIGTERM SIGKILL]", signals)
	}
}

func TestRunDoltCleanup_ForceCountsPostSIGTERMGoneAsReaped(t *testing.T) {
	discoverCalls := 0
	var signals []syscall.Signal

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:      fsys.NewFake(),
		JSON:    true,
		Force:   true,
		HomeDir: "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) {
			discoverCalls++
			switch discoverCalls {
			case 1, 2:
				return []DoltProcInfo{{
					PID:            4444,
					Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"},
					RSSBytes:       4096,
					StartTimeTicks: 10,
				}}, nil
			default:
				return nil, nil
			}
		},
		KillProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	if discoverCalls != 3 {
		t.Fatalf("DiscoverProcesses calls = %d, want initial, pre-SIGTERM, pre-SIGKILL", discoverCalls)
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want [SIGTERM]", signals)
	}
	if r.Reaped.Count != 1 {
		t.Errorf("Reaped.Count = %d, want 1 when process vanishes after our SIGTERM", r.Reaped.Count)
	}
	if r.Summary.BytesFreedRSS != 4096 {
		t.Errorf("Summary.BytesFreedRSS = %d, want 4096", r.Summary.BytesFreedRSS)
	}
	if len(r.Reaped.VanishedPIDs) != 0 {
		t.Errorf("VanishedPIDs = %v, want none for post-SIGTERM success", r.Reaped.VanishedPIDs)
	}
}

func TestRunDoltCleanup_ForceRevalidatesPIDBeforeSIGTERM(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	discoverCalls := 0
	var signals []syscall.Signal

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:    []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:      fs,
		JSON:    true,
		Force:   true,
		HomeDir: "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) {
			discoverCalls++
			if discoverCalls == 1 {
				return []DoltProcInfo{{
					PID:            4444,
					Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"},
					StartTimeTicks: 10,
				}}, nil
			}
			return []DoltProcInfo{{
				PID:            4444,
				Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"},
				Ports:          []int{28231},
				StartTimeTicks: 10,
			}}, nil
		},
		KillProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if discoverCalls != 2 {
		t.Fatalf("DiscoverProcesses called %d time(s), want initial scan plus pre-SIGTERM revalidation", discoverCalls)
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none after PID reclassified as protected before SIGTERM", signals)
	}
	if r.Reaped.Count != 0 {
		t.Errorf("Reaped.Count = %d, want 0 because SIGTERM was skipped", r.Reaped.Count)
	}
	if !equalIntSlice(r.Reaped.ProtectedPIDs, []int{4444}) {
		t.Errorf("ProtectedPIDs = %v, want [4444] after revalidation", r.Reaped.ProtectedPIDs)
	}
}

func TestRunDoltCleanup_ForceSkipsSignalWhenPIDStartTimeChanges(t *testing.T) {
	discoverCalls := 0
	var signals []syscall.Signal

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:      fsys.NewFake(),
		JSON:    true,
		Force:   true,
		HomeDir: "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) {
			discoverCalls++
			if discoverCalls == 1 {
				return []DoltProcInfo{{
					PID:            4444,
					Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"},
					StartTimeTicks: 10,
				}}, nil
			}
			return []DoltProcInfo{{
				PID:            4444,
				Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"},
				StartTimeTicks: 11,
			}}, nil
		},
		KillProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if discoverCalls != 2 {
		t.Fatalf("DiscoverProcesses calls = %d, want 2", discoverCalls)
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none after PID start time changed", signals)
	}
	if r.Reaped.Count != 0 {
		t.Errorf("Reaped.Count = %d, want 0 because PID identity changed", r.Reaped.Count)
	}
	if !equalIntSlice(r.Reaped.ProtectedPIDs, []int{4444}) {
		t.Errorf("ProtectedPIDs = %v, want [4444] after PID identity changed", r.Reaped.ProtectedPIDs)
	}
}

func TestRunDoltCleanup_ForceDoesNotCountMissingPIDAfterRevalidation(t *testing.T) {
	discoverCalls := 0
	var signals []syscall.Signal

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:      fsys.NewFake(),
		JSON:    true,
		Force:   true,
		HomeDir: "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) {
			discoverCalls++
			if discoverCalls == 1 {
				return []DoltProcInfo{{
					PID:            4444,
					Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"},
					StartTimeTicks: 10,
				}}, nil
			}
			return nil, nil
		},
		KillProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if discoverCalls != 2 {
		t.Fatalf("DiscoverProcesses calls = %d, want 2", discoverCalls)
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none when pre-SIGTERM refresh misses the PID", signals)
	}
	if r.Reaped.Count != 0 {
		t.Errorf("Reaped.Count = %d, want 0 because missing-on-refresh is not a confirmed kill", r.Reaped.Count)
	}
	if !equalIntSlice(r.Reaped.VanishedPIDs, []int{4444}) {
		t.Errorf("VanishedPIDs = %v, want [4444]", r.Reaped.VanishedPIDs)
	}
}

func TestRunDoltCleanup_ForceSkipsSIGKILLWhenRevalidationDiscoverErrors(t *testing.T) {
	discoverCalls := 0
	var signals []syscall.Signal

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:      fsys.NewFake(),
		JSON:    true,
		Force:   true,
		HomeDir: "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) {
			discoverCalls++
			if discoverCalls == 1 {
				return []DoltProcInfo{{
					PID:            4444,
					Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"},
					StartTimeTicks: 10,
				}}, nil
			}
			return nil, fmt.Errorf("transient /proc walk failed")
		},
		KillProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if discoverCalls != 2 {
		t.Fatalf("DiscoverProcesses calls = %d, want 2", discoverCalls)
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none when pre-SIGTERM revalidation fails", signals)
	}
	if r.Reaped.Count != 0 {
		t.Errorf("Reaped.Count = %d, want 0 because SIGKILL was skipped", r.Reaped.Count)
	}
	if r.Summary.ErrorsTotal != 1 {
		t.Errorf("Summary.ErrorsTotal = %d, want 1", r.Summary.ErrorsTotal)
	}
	if len(r.Errors) != 1 || r.Errors[0].Stage != "reap" || !strings.Contains(r.Errors[0].Error, "revalidate before SIGTERM") {
		t.Fatalf("Errors = %+v, want revalidation reap error", r.Errors)
	}
}

func TestRunDoltCleanup_ForceRecordsKillError(t *testing.T) {
	procs := []DoltProcInfo{
		{PID: 4444, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"}, StartTimeTicks: 10},
	}
	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		Force:             true,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess: func(_ int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				return nil
			}
			return syscall.EPERM
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(r.Reaped.Errors) == 0 {
		t.Errorf("Reaped.Errors empty; want non-zero kill error")
	}
	if r.Reaped.Count != 0 {
		t.Errorf("Reaped.Count = %d, want 0 because SIGKILL failed", r.Reaped.Count)
	}
	if r.Summary.ErrorsTotal != 1 {
		t.Errorf("Summary.ErrorsTotal = %d, want 1", r.Summary.ErrorsTotal)
	}
	if len(r.Errors) != 1 {
		t.Fatalf("len(Errors) = %d, want 1: %#v", len(r.Errors), r.Errors)
	}
	if r.Errors[0].Stage != "reap" || r.Errors[0].Name != "pid 4444" || !strings.Contains(r.Errors[0].Error, "SIGKILL") {
		t.Errorf("Errors[0] = %#v, want top-level reap SIGKILL error for pid 4444", r.Errors[0])
	}
}

func TestRunDoltCleanup_RigsProtectedReadsDoltDatabaseFromMetadata(t *testing.T) {
	// When a rig's metadata.json sets dolt_database, the protection entry MUST
	// use that value as DB (not the rig name) so the drop step doesn't
	// accidentally target a rig DB whose operator-chosen name differs from
	// the rig's registered name. Falls back to rig.Name when metadata is
	// missing or doesn't specify dolt_database.
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	fs.Files["/rigs/foo/.beads/metadata.json"] = []byte(`{"dolt_database":"foo_db"}`)
	fs.Files["/rigs/bar/.beads/metadata.json"] = []byte(`{"database":"sqlite"}`) // no dolt_database
	// /rigs/missing has no metadata.json at all.

	rigs := []resolverRig{
		{Name: "city", Path: "/city", HQ: true},
		{Name: "foo", Path: "/rigs/foo"},
		{Name: "bar", Path: "/rigs/bar"},
		{Name: "missing", Path: "/rigs/missing"},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:  rigs,
		FS:    fs,
		JSON:  true,
		Probe: false,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runDoltCleanup exit=%d, stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	want := []CleanupRigProtection{
		{Rig: "city", DB: "hq"},         // from metadata
		{Rig: "foo", DB: "foo_db"},      // from metadata
		{Rig: "bar", DB: "bar"},         // metadata present but no dolt_database — fall back to rig.Name
		{Rig: "missing", DB: "missing"}, // no metadata — fall back to rig.Name
	}
	if len(r.RigsProtected) != len(want) {
		t.Fatalf("RigsProtected len = %d, want %d (got %+v)", len(r.RigsProtected), len(want), r.RigsProtected)
	}
	for i, w := range want {
		if r.RigsProtected[i] != w {
			t.Errorf("RigsProtected[%d] = %+v, want %+v", i, r.RigsProtected[i], w)
		}
	}
}

func TestRunDoltCleanup_DryRunReportsUnsafeRigDatabaseName(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/rigs/foo/.beads/metadata.json"] = []byte(`{"dolt_database":"foo db"}`)

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "foo", Path: "/rigs/foo"}},
		FS:                fs,
		JSON:              true,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	if r.Summary.ErrorsTotal != 1 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 1; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
	if len(r.Errors) != 1 || r.Errors[0].Stage != "rig" || r.Errors[0].Name != "foo" || !strings.Contains(r.Errors[0].Error, "foo db") {
		t.Fatalf("Errors = %+v, want typed rig error naming unsafe dolt_database", r.Errors)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
