package beads

import "testing"

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
