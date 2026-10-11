package beads

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkUnreadStoreGuardOnANonEmptyRead is what every read that returns rows
// pays: one atomic load, on a latch that is set once and never cleared.
func BenchmarkUnreadStoreGuardOnANonEmptyRead(b *testing.B) {
	s := NewBdStore(benchScope(b, true), benchEmptyRunner, WithBdStoreNoticeSink(&bytes.Buffer{}))
	s.noteServerRows(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.noteServerRows(1)
	}
}

// BenchmarkUnreadStoreGuardOnAnEmptyRead is what an EMPTY whole-ledger read
// pays once the per-scope verdict has been reached, across the three states a
// live process is in:
//
//   - latched — this scope has answered with a row at some point.
//   - no-second-database — the steady state of every city with one ledger. The
//     verdict cost a metadata read and a stat, once.
//   - notice-already-printed — the scope that DID have a second database. The
//     notice was written once and is never written again, however many stores
//     the process builds over that scope.
//
// All three are two atomic loads: the row latch and the verdict latch. None of
// them is ever a subprocess — see unread_store_notice.go on why a diagnostic
// inside List/Ready may not ask bd anything of its own.
func BenchmarkUnreadStoreGuardOnAnEmptyRead(b *testing.B) {
	latched := NewBdStore(benchScope(b, true), benchEmptyRunner, WithBdStoreNoticeSink(&bytes.Buffer{}))
	latched.noteServerRows(1)

	oneLedger := NewBdStore(benchScope(b, false), benchEmptyRunner, WithBdStoreNoticeSink(&bytes.Buffer{}))
	oneLedger.noticeIfStoreCannotSeeItsLedger("bd ready")

	printed := NewBdStore(benchScope(b, true), benchEmptyRunner, WithBdStoreNoticeSink(&bytes.Buffer{}))
	printed.noticeIfStoreCannotSeeItsLedger("bd ready")

	for name, s := range map[string]*BdStore{
		"latched":                latched,
		"no-second-database":     oneLedger,
		"notice-already-printed": printed,
	} {
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				s.noticeIfStoreCannotSeeItsLedger("bd ready")
			}
		})
	}
}

func benchScope(b *testing.B, withUnread bool) string {
	b.Helper()
	scope := b.TempDir()
	sub := "dolt"
	if withUnread {
		sub = "embeddeddolt"
	}
	if err := os.MkdirAll(filepath.Join(scope, ".beads", sub, "jc", ".dolt"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"jc"}`), 0o644); err != nil {
		b.Fatal(err)
	}
	return scope
}

func benchEmptyRunner(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil }
