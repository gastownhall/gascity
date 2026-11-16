package opjournal_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// The builtin default records only over a SQLite sessions store, found
// through the wrappers production puts around it; a capable store of any
// other engine, Dolt-served or not, answers NotRecordedByDefaultError.
func TestCheckRecordsByDefaultAllowsOnlySQLite(t *testing.T) {
	sq, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.(interface{ CloseStore() error }).CloseStore() })
	if err := opjournal.CheckRecordsByDefault(beads.SessionStore{Store: sq}); err != nil {
		t.Fatalf("SQLite: %v, want nil", err)
	}
	if err := opjournal.CheckRecordsByDefault(beads.SessionStore{Store: beads.NewCachingStoreForTest(sq, nil)}); err != nil {
		t.Fatalf("cached SQLite: %v, want nil", err)
	}

	for name, tc := range map[string]struct {
		store      beads.Store
		doltServed bool
		phrase     string
	}{
		"native Dolt":  {store: beads.NewCachingStoreForTest(&beads.NativeDoltStore{}, nil), doltServed: true, phrase: "a Dolt-served one (NativeDoltStore)"},
		"bd":           {store: &beads.BdStore{}, doltServed: true, phrase: "a Dolt-served one (BdStore)"},
		"memory store": {store: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}, phrase: "not on MemStore"},
	} {
		t.Run(name, func(t *testing.T) {
			err := opjournal.CheckRecordsByDefault(beads.SessionStore{Store: tc.store})
			var notDefault *opjournal.NotRecordedByDefaultError
			if !errors.As(err, &notDefault) {
				t.Fatalf("CheckRecordsByDefault = %v, want *NotRecordedByDefaultError", err)
			}
			if notDefault.Backend.DoltServed != tc.doltServed || notDefault.Backend.SQLite {
				t.Fatalf("backend = %+v, want DoltServed=%v and not SQLite", notDefault.Backend, tc.doltServed)
			}
			// The opt-in is conditional: no I/O ran, so the message cannot
			// know whether this store can record ops (a released bd cannot).
			if msg := err.Error(); !strings.Contains(msg, tc.phrase) || !strings.Contains(msg, "[ops]") ||
				!strings.Contains(msg, "if the store can record ops") {
				t.Fatalf("message %q, want it to name %q and the [ops] opt-in, conditional on the store recording ops", msg, tc.phrase)
			}
		})
	}
}
