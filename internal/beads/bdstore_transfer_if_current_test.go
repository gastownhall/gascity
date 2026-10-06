package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestTransferIfCurrentUsesTheConditionalVerb pins the argv: the transfer is a
// CAS naming the spelling the caller saw, never an unconditional reassign.
func TestTransferIfCurrentUsesTheConditionalVerb(t *testing.T) {
	runner := &releaseVerbRunner{}
	s := beads.NewBdStore("/city", runner.run)

	moved, err := s.TransferIfCurrent("bd-42", "claude-gcg-1", "gcg-1")
	if err != nil || !moved {
		t.Fatalf("TransferIfCurrent = (%v, %v), want (true, nil)", moved, err)
	}
	want := []string{"bd", "update", "bd-42", "--if-assignee", "claude-gcg-1", "--if-status", "in_progress", "--assignee", "gcg-1"}
	calls := runner.releaseVerbArgv()
	if len(calls) != 1 || strings.Join(calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("calls = %q\nwant one %q", calls, want)
	}
}

// A precondition miss is "someone else holds it": false, no error — unless the
// readback shows our own transfer already landed (a retried write).
func TestTransferIfCurrentPreconditionMiss(t *testing.T) {
	for _, tc := range []struct {
		name, holder string
		want         bool
	}{
		{name: "foreign holder", holder: "someone-else", want: false},
		{name: "already transferred", holder: "gcg-1", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &releaseVerbRunner{
				reply: func(args []string) ([]byte, error) {
					if isReleaseVerb(args) {
						return []byte("assignee mismatch"), exitErrorWithCode(t, 13)
					}
					return nil, errors.New("unexpected")
				},
				show: func(id string) ([]byte, error) {
					return []byte(`[{"id":"` + id + `","status":"in_progress","assignee":"` + tc.holder + `"}]`), nil
				},
			}
			s := beads.NewBdStore("/city", runner.run)
			moved, err := s.TransferIfCurrent("bd-42", "claude-gcg-1", "gcg-1")
			if err != nil || moved != tc.want {
				t.Fatalf("TransferIfCurrent = (%v, %v), want (%v, nil)", moved, err, tc.want)
			}
		})
	}
}

// An old bd without --if-assignee must surface as unsupported, never as a
// silent unconditional write or a false "lost".
func TestTransferIfCurrentReportsAnOldBdAsUnsupported(t *testing.T) {
	runner := &releaseVerbRunner{reply: func(_ []string) ([]byte, error) {
		return []byte("Error: unknown flag: --if-assignee"), exitErrorWithDetail(t, 1, "unknown flag: --if-assignee")
	}}
	s := beads.NewBdStore("/city", runner.run)
	moved, err := s.TransferIfCurrent("bd-42", "claude-gcg-1", "gcg-1")
	if moved || !errors.Is(err, beads.ErrConditionalTransferUnsupported) {
		t.Fatalf("TransferIfCurrent = (%v, %v), want (false, ErrConditionalTransferUnsupported)", moved, err)
	}
}
