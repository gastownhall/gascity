package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

func (f *fakeCleanupDoltClientCustomPurge) Close() error { return nil }
