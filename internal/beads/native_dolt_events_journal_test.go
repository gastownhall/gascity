package beads

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/journalops"
)

func TestNativeEventsJournalEnabledForMirrorsBdPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		env    string // "" reads as unset: bd's parser rejects it and falls through
		config string // "" writes no config.yaml
		want   bool
	}{
		{name: "no config is off"},
		{name: "config true", config: "events-journal: true\n", want: true},
		{name: "config yes", config: "events-journal: yes\n", want: true},
		{name: "config quoted on", config: "events-journal: \"on\"\n", want: true},
		{name: "config false", config: "events-journal: false\n"},
		{name: "config unparseable is off", config: "events-journal: maybe\n"},
		{name: "config malformed is off", config: "events-journal: [true\n"},
		{name: "other keys only", config: "issue-prefix: gc\n"},
		{name: "env on beats config off", env: "1", config: "events-journal: false\n", want: true},
		{name: "env off beats config on", env: "off", config: "events-journal: true\n"},
		{name: "env unparseable falls through to config", env: "sure", config: "events-journal: true\n", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BD_EVENTS_JOURNAL", tc.env)
			beadsDir := t.TempDir()
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := nativeEventsJournalEnabledFor(beadsDir); got != tc.want {
				t.Fatalf("nativeEventsJournalEnabledFor = %v, want %v", got, tc.want)
			}
		})
	}
}

// nativeJournalConfigurableSpy is a storage that can journal, recording the
// activation it was given.
type nativeJournalConfigurableSpy struct {
	nativeDoltStorageSpy
	enabled *bool
}

func (s *nativeJournalConfigurableSpy) SetEventsJournalEnabled(enabled bool) { s.enabled = &enabled }

func TestOpenNativeDoltStorageAppliesJournalActivation(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		canJournal bool
		wantErr    bool
		wantSet    bool
	}{
		{name: "enabled on a journaling backend activates it", enabled: true, canJournal: true, wantSet: true},
		{name: "disabled leaves a journaling backend untouched", canJournal: true},
		{name: "disabled accepts a backend that cannot journal"},
		{name: "enabled refuses a backend that cannot journal", enabled: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BD_EVENTS_JOURNAL", "")
			beadsDir := t.TempDir()
			if tc.enabled {
				if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			closed := false
			plain := nativeDoltStorageSpy{close: func() error { closed = true; return nil }}
			journaling := &nativeJournalConfigurableSpy{nativeDoltStorageSpy: plain}
			oldOpen := nativeDoltOpenBestAvailable
			t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
			nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
				if tc.canJournal {
					return journaling, nil
				}
				return &plain, nil
			}

			storage, err := openNativeDoltStorage(context.Background(), beadsDir)
			if tc.wantErr {
				if err == nil || storage != nil || !closed {
					t.Fatalf("openNativeDoltStorage = (%v, %v), closed=%v; want nil storage, an error, and the opened store closed", storage, err, closed)
				}
				return
			}
			if err != nil || closed {
				t.Fatalf("openNativeDoltStorage: err=%v closed=%v", err, closed)
			}
			if gotSet := journaling.enabled != nil && *journaling.enabled; gotSet != tc.wantSet {
				t.Fatalf("journal activated = %v, want %v", gotSet, tc.wantSet)
			}
		})
	}
}

// TestNativeDoltStoreWritesJournalOnlyWhenWorkspaceEnablesIt drives a real
// embedded store through gc's production open: a native write records a
// journal row exactly when the target workspace's config.yaml turns it on.
func TestNativeDoltStoreWritesJournalOnlyWhenWorkspaceEnablesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   bool
	}{
		{name: "enabled", config: "events-journal: true\n", want: true},
		{name: "disabled by default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Embedded Dolt needs an existing private home for its local config.
			t.Setenv("HOME", t.TempDir())
			t.Setenv("BD_EVENTS_JOURNAL", "")
			ctx := context.Background()
			scopeRoot := t.TempDir()
			beadsDir := filepath.Join(scopeRoot, ".beads")
			seed, err := beadslib.OpenBestAvailable(ctx, beadsDir)
			if err != nil {
				t.Skipf("upstream beads storage unavailable: %v", err)
			}
			if err := seed.SetConfig(ctx, nativeIssuePrefixConfigKey, "gc"); err != nil {
				t.Fatalf("SetConfig(issue_prefix): %v", err)
			}
			if err := seed.Close(); err != nil {
				t.Fatalf("close seed storage: %v", err)
			}
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			store, err := OpenNativeDoltStoreAt(ctx, scopeRoot, nil)
			if err != nil {
				t.Fatalf("OpenNativeDoltStoreAt: %v", err)
			}
			t.Cleanup(func() {
				if err := store.CloseStore(); err != nil {
					t.Fatalf("CloseStore: %v", err)
				}
			})
			bead, err := store.Create(Bead{Title: "journal coverage"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			ids := nativeJournalIssueIDs(ctx, t, store.storage)
			if journaled := slices.Contains(ids, bead.ID); journaled != tc.want {
				t.Fatalf("journal recorded %s = %v, want %v; journal issue IDs: %v", bead.ID, journaled, tc.want, ids)
			}
		})
	}
}

// nativeJournalIssueIDs lists the issue IDs the store's journal recorded,
// through the public read role every journaling backend implements.
func nativeJournalIssueIDs(ctx context.Context, t *testing.T, storage beadslib.Storage) []string {
	t.Helper()
	journal, ok := storage.(journalops.Journal)
	if !ok {
		t.Fatalf("%T does not implement journalops.Journal", storage)
	}
	page, err := journal.ReadEventsJournalPage(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ReadEventsJournalPage: %v", err)
	}
	ids := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		ids = append(ids, row.IssueID)
	}
	return ids
}
