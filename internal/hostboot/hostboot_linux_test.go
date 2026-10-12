//go:build linux

package hostboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withProcStatPath points the parser at a fixture for the duration of a test.
func withProcStatPath(t *testing.T, path string) {
	t.Helper()
	prev := procStatPath
	procStatPath = path
	t.Cleanup(func() { procStatPath = prev })
}

// writeProcStat writes contents to a temp file and returns its path.
func writeProcStat(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

// realisticProcStat is a trimmed /proc/stat with btime in its usual position.
const realisticProcStat = `cpu  1 2 3 4 5 6 7 8 9 10
cpu0 1 2 3 4 5 6 7 8 9 10
intr 12345 0 0
ctxt 987654
btime 1700000000
processes 4242
procs_running 2
procs_blocked 0
`

func TestBootTimeParsesProcStat(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     time.Time
		wantErr  string
	}{
		{
			name:     "valid btime",
			contents: realisticProcStat,
			want:     time.Unix(1700000000, 0),
		},
		{
			name:     "btime alone on the first line",
			contents: "btime 1234567890\n",
			want:     time.Unix(1234567890, 0),
		},
		{
			name:     "non-numeric btime",
			contents: "btime not-a-number\n",
			wantErr:  "parsing btime",
		},
		{
			name:     "btime zero",
			contents: "btime 0\n",
			wantErr:  "implausible btime",
		},
		{
			name:     "negative btime",
			contents: "btime -5\n",
			wantErr:  "implausible btime",
		},
		{
			name:     "missing btime line",
			contents: "cpu  1 2 3 4\nctxt 99\n",
			wantErr:  "no btime field",
		},
		{
			name:     "empty file",
			contents: "",
			wantErr:  "no btime field",
		},
		{
			// A three-field "btime" row is not the scalar field we want, so it
			// must not be parsed as one.
			name:     "btime with unexpected arity",
			contents: "btime 1700000000 extra\n",
			wantErr:  "no btime field",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withProcStatPath(t, writeProcStat(t, tt.contents))

			got, err := BootTime()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("BootTime() = %v, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("BootTime() error = %v, want it to contain %q", err, tt.wantErr)
				}
				if !got.IsZero() {
					t.Fatalf("BootTime() = %v on error, want the zero time", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BootTime() error = %v, want nil", err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("BootTime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBootTimeErrorsWhenProcStatUnopenable(t *testing.T) {
	withProcStatPath(t, filepath.Join(t.TempDir(), "does-not-exist"))

	got, err := BootTime()
	if err == nil {
		t.Fatalf("BootTime() = %v, want an error for an unopenable path", got)
	}
	if !strings.Contains(err.Error(), "opening") {
		t.Fatalf("BootTime() error = %v, want it to name the open failure", err)
	}
	if !got.IsZero() {
		t.Fatalf("BootTime() = %v on error, want the zero time", got)
	}
}

// The real host must report a plausible boot instant: in the past, and after
// the epoch. This is the only assertion that exercises the production path.
func TestBootTimeOnRealHost(t *testing.T) {
	got, err := BootTime()
	if err != nil {
		t.Fatalf("BootTime() error = %v, want nil on Linux", err)
	}
	if got.After(time.Now()) {
		t.Fatalf("BootTime() = %v, want an instant in the past", got)
	}
	if got.Unix() <= 0 {
		t.Fatalf("BootTime() = %v, want a positive epoch second", got)
	}
}
