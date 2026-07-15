package beads

import (
	"errors"
	"testing"
)

// TestIsBdTransientWriteError pins the write-retry classifier's needle set.
// A bd backed by sqlite (modernc.org/sqlite) surfaces
// "database is locked (5) (SQLITE_BUSY)" on lock contention — the textbook
// transient write failure, which must be retried. Only the explicit
// SQLITE_BUSY / SQLITE_LOCKED code markers match: bare "database is locked"
// phrasings must NOT be retried, because Dolt's embedded mode uses that
// phrasing for a persistent lock-file condition. Constraint and syntax
// errors are permanent and must NOT be retried either.
func TestIsBdTransientWriteError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},

		// sqlite busy/locked code markers: transient, retried.
		{name: "sqlite database locked", err: errors.New("exit status 1: creating issue: database is locked (5) (SQLITE_BUSY)"), want: true},
		{name: "sqlite table locked", err: errors.New("exit status 1: updating issue: database table is locked (6) (SQLITE_LOCKED)"), want: true},
		{name: "sqlite busy bare code", err: errors.New("exit status 1: SQLITE_BUSY: database busy"), want: true},

		// Lock phrasings without a sqlite code marker: NOT retried. Dolt's
		// embedded mode holds a lock file for the life of another process;
		// a bounded retry cannot clear it and must keep failing fast.
		{name: "dolt embedded lock file", err: errors.New("exit status 1: database is locked by another dolt process"), want: false},
		{name: "bare database is locked", err: errors.New("exit status 1: database is locked"), want: false},

		// sqlite permanent errors: NOT retried.
		{name: "sqlite unique constraint", err: errors.New("exit status 1: constraint failed: UNIQUE constraint failed: issues.id (1555) (SQLITE_CONSTRAINT_UNIQUE)"), want: false},
		{name: "sqlite syntax error", err: errors.New(`exit status 1: SQL logic error: near "FROM": syntax error (1) (SQLITE_ERROR)`), want: false},

		// Existing dolt/mysql needles: unchanged.
		{name: "dolt serialization failure", err: errors.New("exit status 1: Error 1213 (40001): serialization failure"), want: true},
		{name: "dolt committed transaction conflict", err: errors.New("exit status 1: this transaction conflicts with a committed transaction"), want: true},
		{name: "dolt catalog prepare failure", err: errors.New("exit status 1: failed to prepare catalog"), want: true},
		{name: "mysql invalid connection", err: errors.New("exit status 1: [mysql] invalid connection"), want: true},
		{name: "broken pipe", err: errors.New("exit status 1: write: broken pipe"), want: true},

		// Generic permanent errors: unchanged.
		{name: "plain bd failure", err: errors.New("exit status 1: no issue found bd-42"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBdTransientWriteError(tt.err); got != tt.want {
				t.Fatalf("isBdTransientWriteError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestBdStdoutErrorDetail(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "empty",
			out:  "",
			want: "",
		},
		{
			name: "non json",
			out:  "bd failed",
			want: "",
		},
		{
			name: "malformed json",
			out:  `{"error":`,
			want: "",
		},
		{
			name: "missing error",
			out:  `{"schema_version":1}`,
			want: "",
		},
		{
			name: "null error",
			out:  `{"error":null,"schema_version":1}`,
			want: "",
		},
		{
			name: "blank error",
			out:  `{"error":"   ","schema_version":1}`,
			want: "",
		},
		{
			name: "error envelope",
			out:  `{"error":" no issue found bd-42 ","schema_version":1}`,
			want: "no issue found bd-42",
		},
		{
			name: "preamble before envelope",
			out:  "bd warning before json\n{\"error\":\"resolving dependency: no issue found bd-42\",\"schema_version\":1}",
			want: "resolving dependency: no issue found bd-42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bdStdoutErrorDetail([]byte(tt.out)); got != tt.want {
				t.Fatalf("bdStdoutErrorDetail() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBdCloseArgsAlwaysForce pins --force in the close arg shape. The stale
// order-wisp batch sweep (cmd/gc closeStaleOrderWispIDs) closes batches
// without in-batch blocks ordering; that is safe only because bd closes
// blocked beads under --force regardless of order, while a non-forced
// wrong-order batch silently skips blocked beads and exits 0. Removing
// --force would silently resurface the orphaned-step failure mode (#1420).
func TestBdCloseArgsAlwaysForce(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		ids    []string
		want   []string
	}{
		{
			name: "single id no reason",
			ids:  []string{"bd-1"},
			want: []string{"close", "--force", "--json", "bd-1"},
		},
		{
			name:   "batch with reason",
			reason: "stale order wisp sweep",
			ids:    []string{"bd-1", "bd-2", "bd-3"},
			want:   []string{"close", "--force", "--json", "--reason", "stale order wisp sweep", "bd-1", "bd-2", "bd-3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bdCloseArgs(tt.reason, tt.ids...)
			if len(got) != len(tt.want) {
				t.Fatalf("bdCloseArgs = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("bdCloseArgs = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
