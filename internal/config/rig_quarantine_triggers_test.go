//go:build !windows

package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A cache whose `git rev-parse HEAD` fails (issue #6656 trigger 1, an
// unaccepted Xcode license) quarantines its rig like a dirty cache does.
func TestLoadIsolateRigPackFailuresQuarantinesRevParseFailure(t *testing.T) {
	f := newRigQuarantineFixture(t)
	orig := runRepoCacheGit
	runRepoCacheGit = func(dir string, args ...string) (string, error) {
		if dir == f.cacheDir && len(args) > 0 && args[0] == "rev-parse" {
			return "", fmt.Errorf("git rev-parse HEAD: exit status 69: Xcode license not accepted")
		}
		return orig(dir, args...)
	}
	t.Cleanup(func() { runRepoCacheGit = orig })

	cfg, _ := loadIsolated(t, f)
	assertBadRigQuarantined(t, f, cfg, "Xcode license not accepted")
}

// A held repo-cache lock is contention, not a verdict on the cache: the load
// reports ErrRepoCacheBusy and quarantines nothing.
func TestLoadIsolateRigPackFailuresDoesNotQuarantineBusyCache(t *testing.T) {
	f := newRigQuarantineFixture(t)
	lockRepoCacheRoot(t, filepath.Dir(f.cacheDir), syscall.LOCK_EX)

	type result struct {
		cfg *City
		err error
	}
	done := make(chan result, 1)
	go func() {
		cfg, _, err := LoadWithIncludesOptions(fsys.OSFS{}, f.cityToml(),
			LoadOptions{IsolateRigPackFailures: true, RepoCacheNonBlocking: true})
		done <- result{cfg, err}
	}()

	select {
	case r := <-done:
		if !errors.Is(r.err, ErrRepoCacheBusy) {
			t.Fatalf("err = %v, want ErrRepoCacheBusy", r.err)
		}
		if r.cfg != nil && len(r.cfg.QuarantinedRigs) != 0 {
			t.Errorf("QuarantinedRigs = %+v, want none for a busy cache", r.cfg.QuarantinedRigs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("non-blocking isolated load waited on a held repo-cache lock")
	}
}
