package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// putFakeDirTree adds a directory tree with given file sizes to the fake FS.
// Files map values are dummy bytes of the requested length so Stat reports
// the right size.
func putFakeDirTree(fs *fsys.Fake, root string, fileSizes map[string]int64) {
	fs.Dirs[root] = true
	for relPath, size := range fileSizes {
		full := root + "/" + relPath
		// Mark intermediate dirs.
		for d := full; d != root && d != "." && d != "/"; d = parentDir(d) {
			parent := parentDir(d)
			if parent == "" || parent == "." {
				break
			}
			fs.Dirs[parent] = true
			if parent == root {
				break
			}
		}
		fs.Files[full] = make([]byte, size)
	}
}

func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if i == 0 {
				return "/"
			}
			return p[:i]
		}
	}
	return ""
}

// putCanonicalCityConfig writes a city_canonical scope config into the fake
// FS so contract.ResolveDoltConnectionTarget resolves the city endpoint from
// config alone (no managed runtime state, no network).
func putCanonicalCityConfig(fs *fsys.Fake) {
	fs.Files["/city/.beads/config.yaml"] = []byte("issue_prefix: gc\n" +
		"gc.endpoint_origin: city_canonical\n" +
		"gc.endpoint_status: verified\n" +
		"dolt.host: 127.0.0.1\n" +
		"dolt.port: \"28231\"\n")
}

// putExplicitRigConfig writes an explicit-endpoint rig scope config into the
// fake FS.
func putExplicitRigConfig(fs *fsys.Fake, rigPath, port string) {
	fs.Files[rigPath+"/.beads/config.yaml"] = []byte("issue_prefix: r\n" +
		"gc.endpoint_origin: explicit\n" +
		"gc.endpoint_status: verified\n" +
		"dolt.host: 127.0.0.1\n" +
		"dolt.port: \"" + port + "\"\n")
}

func TestRunDoltCleanup_DryRunComputesPurgeBytesFromDroppedDirs(t *testing.T) {
	fs := fsys.NewFake()
	// City rig has 3 dropped databases on disk, total 3000 bytes.
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin":     1000,
		"db_b/manifest":     500,
		"db_b/blob/abc.dat": 500,
		"db_c/index":        1000,
	})
	// HQ metadata so the rig protection enumerates with DB="hq".
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)

	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}
	client := &fakeCleanupDoltClient{databases: []string{"hq"}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		JSON:              true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Purge.BytesReclaimed != 3000 {
		t.Errorf("Purge.BytesReclaimed = %d, want 3000", r.Purge.BytesReclaimed)
	}
	if client.purged != 0 {
		t.Errorf("PurgeDroppedDatabases called %d times in dry-run; want 0", client.purged)
	}
}

func TestRunDoltCleanup_ForceCallsPurgePerRigDatabase(t *testing.T) {
	fs := fsys.NewFake()
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})
	putFakeDirTree(fs, "/rigs/foo/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_b/data.bin": 200,
	})
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	fs.Files["/rigs/foo/.beads/metadata.json"] = []byte(`{"dolt_database":"foo_db"}`)

	rigs := []resolverRig{
		{Name: "city", Path: "/city", HQ: true},
		{Name: "foo", Path: "/rigs/foo"},
	}
	purgedNames := []string{}
	client := &fakeCleanupDoltClientCustomPurge{
		databases: []string{"hq", "foo_db"},
		onPurge:   func(name string) error { purgedNames = append(purgedNames, name); return nil },
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !r.Purge.OK {
		t.Errorf("Purge.OK = false, want true")
	}
	if r.Purge.BytesReclaimed != 300 {
		t.Errorf("Purge.BytesReclaimed = %d, want 300", r.Purge.BytesReclaimed)
	}
	wantPurged := []string{"hq", "foo_db"}
	if !equalStringSlice(purgedNames, wantPurged) {
		t.Errorf("purged DBs = %v, want %v", purgedNames, wantPurged)
	}
}

func TestRunDoltCleanup_PurgeFailureRecordedNotFatal(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})

	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}
	client := &fakeCleanupDoltClientCustomPurge{
		databases: []string{"hq"},
		onPurge:   func(_ string) error { return fmt.Errorf("purge boom") },
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Purge.OK {
		t.Errorf("Purge.OK = true, want false (purge failed)")
	}
	if r.Purge.BytesReclaimed != 0 {
		t.Errorf("Purge.BytesReclaimed = %d, want 0 because purge failed", r.Purge.BytesReclaimed)
	}
	if r.Summary.BytesFreedDisk != 0 {
		t.Errorf("Summary.BytesFreedDisk = %d, want 0 because purge failed", r.Summary.BytesFreedDisk)
	}
	hasPurgeError := false
	for _, e := range r.Errors {
		if e.Stage == "purge" && strings.Contains(e.Error, "purge boom") {
			hasPurgeError = true
		}
	}
	if !hasPurgeError {
		t.Errorf("Errors missing purge entry: %+v", r.Errors)
	}
}

func TestRunDoltCleanup_ForceFailsPurgeWhenMissingRigDatabaseHasBytes(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})

	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}
	client := &fakeCleanupDoltClientCustomPurge{
		databases: []string{"other"},
		onPurge: func(name string) error {
			t.Fatalf("PurgeDroppedDatabases(%q) called for missing database", name)
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	if r.Purge.OK {
		t.Errorf("Purge.OK = true, want false when reclaimable bytes belong to a non-live database")
	}
	if r.Purge.BytesReclaimed != 0 {
		t.Errorf("Purge.BytesReclaimed = %d, want 0 because no purge call succeeded", r.Purge.BytesReclaimed)
	}
	if r.Summary.ErrorsTotal != 1 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 1; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
	if len(r.Errors) != 1 || r.Errors[0].Stage != "purge" || r.Errors[0].Name != "hq" || !strings.Contains(r.Errors[0].Error, "not live") {
		t.Fatalf("Errors = %+v, want purge error for missing live database hq", r.Errors)
	}
}

func TestRunDoltCleanup_PurgeReportsUnexpectedFilesystemErrors(t *testing.T) {
	fs := fsys.NewFake()
	root := "/city/.beads/dolt/.dolt_dropped_databases"
	fs.Errors[root] = os.ErrPermission
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "city", Path: "/city", HQ: true}},
		FS:                fs,
		JSON:              true,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if r.Purge.BytesReclaimed != 0 {
		t.Fatalf("Purge.BytesReclaimed = %d, want 0 when dropped-db walk failed", r.Purge.BytesReclaimed)
	}
	if r.Summary.ErrorsTotal != 1 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 1; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
	if len(r.Errors) != 1 || r.Errors[0].Stage != "purge" || r.Errors[0].Name != root || !strings.Contains(r.Errors[0].Error, "permission") {
		t.Fatalf("Errors = %+v, want purge filesystem permission error for %s", r.Errors, root)
	}
}

func TestSQLCleanupDoltClientPurgePinsUseAndCallToOneConnection(t *testing.T) {
	resetPurgeConnRecorder()

	db, err := sql.Open("gc_cleanup_purge_conn_recorder", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	db.SetMaxIdleConns(0)

	client := &sqlCleanupDoltClient{db: db}
	if err := client.PurgeDroppedDatabases(context.Background(), "rig_db"); err != nil {
		t.Fatalf("PurgeDroppedDatabases: %v", err)
	}

	execs := purgeConnRecorderExecs()
	if len(execs) != 2 {
		t.Fatalf("execs = %+v, want USE and CALL", execs)
	}
	if execs[0].query != "USE `rig_db`" {
		t.Errorf("first query = %q, want USE `rig_db`", execs[0].query)
	}
	if execs[1].query != "CALL DOLT_PURGE_DROPPED_DATABASES()" {
		t.Errorf("second query = %q, want CALL DOLT_PURGE_DROPPED_DATABASES()", execs[1].query)
	}
	if execs[0].connID != execs[1].connID {
		t.Fatalf("USE ran on conn %d and CALL ran on conn %d; want one pinned connection", execs[0].connID, execs[1].connID)
	}
}

func TestRunDoltCleanup_ForceSkipsPurgeForMissingRigDatabases(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	fs.Files["/rigs/fresh/.beads/metadata.json"] = []byte(`{"dolt_database":"fresh_db"}`)

	rigs := []resolverRig{
		{Name: "city", Path: "/city", HQ: true},
		{Name: "fresh", Path: "/rigs/fresh"},
	}
	purgedNames := []string{}
	client := &fakeCleanupDoltClientCustomPurge{
		databases: []string{"hq"},
		onPurge:   func(name string) error { purgedNames = append(purgedNames, name); return nil },
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !r.Purge.OK {
		t.Errorf("Purge.OK = false, want true")
	}
	wantPurged := []string{"hq"}
	if !equalStringSlice(purgedNames, wantPurged) {
		t.Errorf("purged DBs = %v, want %v", purgedNames, wantPurged)
	}
	if r.Summary.ErrorsTotal != 0 {
		t.Errorf("Summary.ErrorsTotal = %d, want 0; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
}

func TestRunDoltCleanup_ForceSkipsPurgeBytesForRigsOnDifferentPort(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	putCanonicalCityConfig(fs)
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})
	fs.Files["/rigs/other/.beads/metadata.json"] = []byte(`{"dolt_database":"other_db"}`)
	putExplicitRigConfig(fs, "/rigs/other", "28232")
	putFakeDirTree(fs, "/rigs/other/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_b/data.bin": 200,
	})

	rigs := []resolverRig{
		{Name: "city", Path: "/city", HQ: true},
		{Name: "other", Path: "/rigs/other"},
	}
	purgedNames := []string{}
	client := &fakeCleanupDoltClientCustomPurge{
		databases: []string{"hq"},
		onPurge:   func(name string) error { purgedNames = append(purgedNames, name); return nil },
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		CityPath:          "/city",
		PortResolution:    PortResolution{Port: 28231, Source: liveDoltHandleSource},
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if !r.Purge.OK {
		t.Errorf("Purge.OK = false, want true because non-resolved server was skipped")
	}
	if r.Purge.BytesReclaimed != 100 {
		t.Errorf("Purge.BytesReclaimed = %d, want only resolved-server bytes", r.Purge.BytesReclaimed)
	}
	if r.Summary.ErrorsTotal != 0 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 0; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
	wantPurged := []string{"hq"}
	if !equalStringSlice(purgedNames, wantPurged) {
		t.Errorf("purged DBs = %v, want %v", purgedNames, wantPurged)
	}
}

func TestRunDoltCleanup_DryRunSkipsPurgeBytesForRigsOnDifferentPort(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	putCanonicalCityConfig(fs)
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})
	fs.Files["/rigs/other/.beads/metadata.json"] = []byte(`{"dolt_database":"other_db"}`)
	putExplicitRigConfig(fs, "/rigs/other", "28232")
	putFakeDirTree(fs, "/rigs/other/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_b/data.bin": 200,
	})

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs: []resolverRig{
			{Name: "city", Path: "/city", HQ: true},
			{Name: "other", Path: "/rigs/other"},
		},
		FS:                fs,
		CityPath:          "/city",
		PortResolution:    PortResolution{Port: 28231, Source: liveDoltHandleSource},
		JSON:              true,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if r.Purge.BytesReclaimed != 100 {
		t.Fatalf("Purge.BytesReclaimed = %d, want only resolved-server bytes", r.Purge.BytesReclaimed)
	}
	if r.Summary.ErrorsTotal != 0 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 0; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
}

func TestRunDoltCleanup_DryRunCountsCityConfigPortRigWithoutPortFile(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})
	fs.Files["/rigs/unknown/.beads/metadata.json"] = []byte(`{"dolt_database":"unknown_db"}`)
	putFakeDirTree(fs, "/rigs/unknown/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_b/data.bin": 200,
	})

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs: []resolverRig{
			{Name: "city", Path: "/city", HQ: true},
			{Name: "unknown", Path: "/rigs/unknown"},
		},
		CityPort:          28231,
		PortResolution:    PortResolution{Port: 28231, Source: cityConfigDoltPortSource},
		FS:                fs,
		JSON:              true,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if r.Purge.BytesReclaimed != 300 {
		t.Fatalf("Purge.BytesReclaimed = %d, want city-config inherited rig bytes included", r.Purge.BytesReclaimed)
	}
	if r.Summary.ErrorsTotal != 0 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 0; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
}

func TestRunDoltCleanup_ForcePurgesCityConfigPortRigWithoutPortFile(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})
	fs.Files["/rigs/unknown/.beads/metadata.json"] = []byte(`{"dolt_database":"unknown_db"}`)
	putFakeDirTree(fs, "/rigs/unknown/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_b/data.bin": 200,
	})

	purgedNames := []string{}
	client := &fakeCleanupDoltClientCustomPurge{
		databases: []string{"hq", "unknown_db"},
		onPurge:   func(name string) error { purgedNames = append(purgedNames, name); return nil },
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs: []resolverRig{
			{Name: "city", Path: "/city", HQ: true},
			{Name: "unknown", Path: "/rigs/unknown"},
		},
		CityPort:          28231,
		PortResolution:    PortResolution{Port: 28231, Source: cityConfigDoltPortSource},
		FS:                fs,
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if !r.Purge.OK {
		t.Errorf("Purge.OK = false, want true")
	}
	if r.Purge.BytesReclaimed != 300 {
		t.Fatalf("Purge.BytesReclaimed = %d, want city-config inherited rig bytes included", r.Purge.BytesReclaimed)
	}
	wantPurged := []string{"hq", "unknown_db"}
	if !equalStringSlice(purgedNames, wantPurged) {
		t.Fatalf("purged DBs = %v, want %v", purgedNames, wantPurged)
	}
	if r.Summary.ErrorsTotal != 0 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 0; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
}

// invalidRigEndpointConfigCases enumerates canonical rig configs that must
// fail closed in rigSharesResolvedDoltServer (excluded from purge accounting)
// even when the city-config port would otherwise vouch for unresolvable rigs.
func invalidRigEndpointConfigCases() []struct {
	name  string
	setup func(*fsys.Fake)
} {
	configFile := "/rigs/unknown/.beads/config.yaml"
	return []struct {
		name  string
		setup func(*fsys.Fake)
	}{
		{
			name: "explicit without port",
			setup: func(fs *fsys.Fake) {
				fs.Files[configFile] = []byte("issue_prefix: r\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\n")
			},
		},
		{
			name: "bind-address host",
			setup: func(fs *fsys.Fake) {
				fs.Files[configFile] = []byte("issue_prefix: r\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: 0.0.0.0\ndolt.port: \"28231\"\n")
			},
		},
		{
			name: "unreadable config",
			setup: func(fs *fsys.Fake) {
				fs.Files[configFile] = []byte("gc.endpoint_origin: explicit\n")
				fs.Errors[configFile] = os.ErrPermission
			},
		},
	}
}

func TestRunDoltCleanup_DryRunSkipsPurgeBytesForInvalidRigEndpointConfigWithCityConfig(t *testing.T) {
	for _, tt := range invalidRigEndpointConfigCases() {
		t.Run(tt.name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
			putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
				"db_a/data.bin": 100,
			})
			fs.Files["/rigs/unknown/.beads/metadata.json"] = []byte(`{"dolt_database":"unknown_db"}`)
			putFakeDirTree(fs, "/rigs/unknown/.beads/dolt/.dolt_dropped_databases", map[string]int64{
				"db_b/data.bin": 200,
			})
			tt.setup(fs)

			var stdout, stderr bytes.Buffer
			opts := cleanupOptions{
				Rigs: []resolverRig{
					{Name: "city", Path: "/city", HQ: true},
					{Name: "unknown", Path: "/rigs/unknown"},
				},
				CityPort:          28231,
				CityPath:          "/city",
				PortResolution:    PortResolution{Port: 28231, Source: cityConfigDoltPortSource},
				FS:                fs,
				JSON:              true,
				DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
			}
			code := runDoltCleanup(opts, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
			}
			var r CleanupReport
			if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
				t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
			}
			if r.Purge.BytesReclaimed != 100 {
				t.Fatalf("Purge.BytesReclaimed = %d, want only city rig bytes", r.Purge.BytesReclaimed)
			}
			if r.Summary.ErrorsTotal != 0 {
				t.Fatalf("Summary.ErrorsTotal = %d, want 0; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
			}
		})
	}
}

func TestRunDoltCleanup_ForceSkipsPurgeForInvalidRigEndpointConfigWithCityConfig(t *testing.T) {
	for _, tt := range invalidRigEndpointConfigCases() {
		t.Run(tt.name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
			putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
				"db_a/data.bin": 100,
			})
			fs.Files["/rigs/unknown/.beads/metadata.json"] = []byte(`{"dolt_database":"unknown_db"}`)
			putFakeDirTree(fs, "/rigs/unknown/.beads/dolt/.dolt_dropped_databases", map[string]int64{
				"db_b/data.bin": 200,
			})
			tt.setup(fs)

			purgedNames := []string{}
			client := &fakeCleanupDoltClientCustomPurge{
				databases: []string{"hq", "unknown_db"},
				onPurge:   func(name string) error { purgedNames = append(purgedNames, name); return nil },
			}

			var stdout, stderr bytes.Buffer
			opts := cleanupOptions{
				Rigs: []resolverRig{
					{Name: "city", Path: "/city", HQ: true},
					{Name: "unknown", Path: "/rigs/unknown"},
				},
				CityPort:          28231,
				CityPath:          "/city",
				PortResolution:    PortResolution{Port: 28231, Source: cityConfigDoltPortSource},
				FS:                fs,
				JSON:              true,
				Force:             true,
				DoltClient:        client,
				DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
				ReapGracePeriod:   1,
			}
			code := runDoltCleanup(opts, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
			}
			var r CleanupReport
			if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
				t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
			}
			if !r.Purge.OK {
				t.Errorf("Purge.OK = false, want true")
			}
			if r.Purge.BytesReclaimed != 100 {
				t.Fatalf("Purge.BytesReclaimed = %d, want only city rig bytes", r.Purge.BytesReclaimed)
			}
			wantPurged := []string{"hq"}
			if !equalStringSlice(purgedNames, wantPurged) {
				t.Fatalf("purged DBs = %v, want %v", purgedNames, wantPurged)
			}
			if r.Summary.ErrorsTotal != 0 {
				t.Fatalf("Summary.ErrorsTotal = %d, want 0; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
			}
		})
	}
}

func TestRunDoltCleanup_ForceSkipsPurgeWhenScopeSharingUnproven(t *testing.T) {
	// No canonical configs anywhere and no managed runtime state: nothing
	// proves which scopes share the resolved server, and the resolution
	// source is neither the legacy fallback nor the city config. Every scope
	// must be excluded from purge accounting — the old port-file vouching is
	// gone (city-scale plan P1.7).
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})
	fs.Files["/rigs/unknown/.beads/metadata.json"] = []byte(`{"dolt_database":"unknown_db"}`)
	putFakeDirTree(fs, "/rigs/unknown/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_b/data.bin": 200,
	})

	purgedNames := []string{}
	client := &fakeCleanupDoltClientCustomPurge{
		databases: []string{"hq", "unknown_db"},
		onPurge:   func(name string) error { purgedNames = append(purgedNames, name); return nil },
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs: []resolverRig{
			{Name: "city", Path: "/city", HQ: true},
			{Name: "unknown", Path: "/rigs/unknown"},
		},
		FS:                fs,
		CityPath:          "/city",
		PortResolution:    PortResolution{Port: 28231, Source: liveDoltHandleSource},
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if r.Purge.BytesReclaimed != 0 {
		t.Fatalf("Purge.BytesReclaimed = %d, want 0 when no scope provably shares the resolved server", r.Purge.BytesReclaimed)
	}
	if len(purgedNames) != 0 {
		t.Fatalf("purged DBs = %v, want none", purgedNames)
	}
	if r.Summary.ErrorsTotal != 0 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 0; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
}

func TestRigSharesResolvedDoltServer_ContractResolutionDecides(t *testing.T) {
	resolved := PortResolution{Port: 28231, Source: liveDoltHandleSource}

	t.Run("city scope with canonical endpoint shares", func(t *testing.T) {
		fs := fsys.NewFake()
		putCanonicalCityConfig(fs)
		opts := cleanupOptions{FS: fs, CityPath: "/city", PortResolution: resolved}
		if !rigSharesResolvedDoltServer(resolverRig{Name: "city", Path: "/city", HQ: true}, opts) {
			t.Error("city scope on the resolved canonical port must share")
		}
	})

	t.Run("inherited rig under canonical city shares", func(t *testing.T) {
		fs := fsys.NewFake()
		putCanonicalCityConfig(fs)
		opts := cleanupOptions{FS: fs, CityPath: "/city", PortResolution: resolved}
		// Rig has no canonical config: it inherits the city endpoint, which
		// provably matches the resolved port.
		if !rigSharesResolvedDoltServer(resolverRig{Name: "r", Path: "/rigs/r"}, opts) {
			t.Error("inherited rig under canonical city must share")
		}
	})

	t.Run("explicit rig on a different port does not share", func(t *testing.T) {
		fs := fsys.NewFake()
		putCanonicalCityConfig(fs)
		putExplicitRigConfig(fs, "/rigs/r", "28232")
		opts := cleanupOptions{FS: fs, CityPath: "/city", PortResolution: resolved}
		if rigSharesResolvedDoltServer(resolverRig{Name: "r", Path: "/rigs/r"}, opts) {
			t.Error("explicit rig on another port must not share")
		}
	})

	t.Run("invalid rig config fails closed", func(t *testing.T) {
		fs := fsys.NewFake()
		putCanonicalCityConfig(fs)
		fs.Files["/rigs/r/.beads/config.yaml"] = []byte("issue_prefix: r\ngc.endpoint_origin: explicit\n")
		opts := cleanupOptions{FS: fs, CityPath: "/city", PortResolution: resolved}
		if rigSharesResolvedDoltServer(resolverRig{Name: "r", Path: "/rigs/r"}, opts) {
			t.Error("invalid canonical rig config must fail closed")
		}
	})

	t.Run("unresolvable managed scope keeps conservative fallback semantics", func(t *testing.T) {
		fs := fsys.NewFake()
		opts := cleanupOptions{
			FS:             fs,
			CityPath:       "/city",
			CityPort:       28231,
			PortResolution: PortResolution{Port: 28231, Source: cityConfigDoltPortSource},
		}
		if !rigSharesResolvedDoltServer(resolverRig{Name: "r", Path: "/rigs/r"}, opts) {
			t.Error("city-config-selected port must vouch for unresolvable scopes")
		}
		opts.PortResolution = PortResolution{Port: 28231, Source: liveDoltHandleSource}
		if rigSharesResolvedDoltServer(resolverRig{Name: "r", Path: "/rigs/r"}, opts) {
			t.Error("non-fallback, non-city-config sources must not vouch for unresolvable scopes")
		}
	})
}

// fakeCleanupDoltClientCustomPurge is like fakeCleanupDoltClient but lets a
// test inject custom purge behavior so it can exercise failure paths and
// observe call order.
type fakeCleanupDoltClientCustomPurge struct {
	databases []string
	onPurge   func(name string) error
}

func (f *fakeCleanupDoltClientCustomPurge) ListDatabases(_ context.Context) ([]string, error) {
	return append([]string{}, f.databases...), nil
}

func (f *fakeCleanupDoltClientCustomPurge) DropDatabase(_ context.Context, _ string) error {
	return nil
}

func (f *fakeCleanupDoltClientCustomPurge) PurgeDroppedDatabases(_ context.Context, name string) error {
	if f.onPurge != nil {
		return f.onPurge(name)
	}
	return nil
}

func (f *fakeCleanupDoltClientCustomPurge) ProbeLiveSessions(_ context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}

func (f *fakeCleanupDoltClientCustomPurge) Close() error { return nil }

type purgeConnRecord struct {
	connID int
	query  string
}

var purgeConnRecorder = struct {
	sync.Mutex
	nextConnID int
	execs      []purgeConnRecord
}{}

func init() {
	sql.Register("gc_cleanup_purge_conn_recorder", purgeConnRecorderDriver{})
}

func resetPurgeConnRecorder() {
	purgeConnRecorder.Lock()
	defer purgeConnRecorder.Unlock()
	purgeConnRecorder.nextConnID = 0
	purgeConnRecorder.execs = nil
}

func purgeConnRecorderExecs() []purgeConnRecord {
	purgeConnRecorder.Lock()
	defer purgeConnRecorder.Unlock()
	out := make([]purgeConnRecord, len(purgeConnRecorder.execs))
	copy(out, purgeConnRecorder.execs)
	return out
}

type purgeConnRecorderDriver struct{}

func (purgeConnRecorderDriver) Open(_ string) (driver.Conn, error) {
	purgeConnRecorder.Lock()
	defer purgeConnRecorder.Unlock()
	purgeConnRecorder.nextConnID++
	return &purgeConnRecorderConn{id: purgeConnRecorder.nextConnID}, nil
}

type purgeConnRecorderConn struct {
	id int
}

func (c *purgeConnRecorderConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}

func (c *purgeConnRecorderConn) Close() error { return nil }

func (c *purgeConnRecorderConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions unsupported")
}

func (c *purgeConnRecorderConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	purgeConnRecorder.Lock()
	defer purgeConnRecorder.Unlock()
	purgeConnRecorder.execs = append(purgeConnRecorder.execs, purgeConnRecord{connID: c.id, query: query})
	return driver.RowsAffected(0), nil
}
