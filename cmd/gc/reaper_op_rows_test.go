package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/rollout"
)

// fakeReaperScopeBd answers the reaper guard's bd reads for one scope and
// records each call.
type fakeReaperScopeBd struct {
	census    string
	censusErr error
	help      string
	retained  string
	calls     []string
}

func (f *fakeReaperScopeBd) run(dir, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, dir+": "+name+" "+strings.Join(args, " "))
	switch {
	case len(args) > 0 && args[0] == "sql":
		if f.censusErr != nil {
			return nil, f.censusErr
		}
		return []byte("COUNT(*)\n" + f.census + "\n"), nil
	case len(args) == 2 && args[1] == "--help":
		return []byte("bd " + args[0] + " help\n" + f.help + "\n"), nil
	case len(args) == 4 && args[0] == "config" && args[1] == "get":
		return []byte(`{"key":"` + args[3] + `","value":"` + strings.ReplaceAll(f.retained, `"`, `\"`) + `"}`), nil
	}
	return nil, errors.New("unexpected bd call")
}

func (f *fakeReaperScopeBd) scope() reaperOpRowsScope {
	return reaperOpRowsScope{label: "city", dir: "/city", run: f.run}
}

const honorsRetainedTypesHelp = "Without --retain-ids it also skips beads whose type the workspace's types.retained setting names"

func TestReaperOpRowsHeldBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bd      fakeReaperScopeBd
		want    string
		wantErr string
	}{
		{"no op rows", fakeReaperScopeBd{census: "0"}, "", ""},
		{"op rows, bd does not honor types.retained", fakeReaperScopeBd{census: "1", retained: "op"}, "holds op rows and its bd does not honor types.retained", ""},
		{"op rows, types.retained unset", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp}, "holds op rows and types.retained does not name op", ""},
		{"op rows, types.retained names other types", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: "operation,molecule"}, "holds op rows and types.retained does not name op", ""},
		{"op rows, types.retained names op", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: "molecule, op"}, "", ""},
		{"op rows, types.retained a JSON list naming op", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: `["op"]`}, "", ""},
		{"census fails", fakeReaperScopeBd{censusErr: errors.New("dolt down")}, "", "counting op rows: dolt down"},
		{"census unreadable", fakeReaperScopeBd{census: "id"}, "", `counting op rows: the census returned "COUNT(*)\nid"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := tc.bd
			got, err := reaperOpRowsHeldBack(bd.scope())
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if got != tc.want || gotErr != tc.wantErr {
				t.Fatalf("reaperOpRowsHeldBack = %q, %q; want %q, %q (calls %q)", got, gotErr, tc.want, tc.wantErr, bd.calls)
			}
		})
	}
}

// TestReaperOpRowsHeldBackProbesOnlyAScopeWithOpRows: a scope without op rows
// costs one bd read; one with them asks both verbs' help and the setting.
func TestReaperOpRowsHeldBackProbesOnlyAScopeWithOpRows(t *testing.T) {
	none := fakeReaperScopeBd{census: "0"}
	_, _ = reaperOpRowsHeldBack(none.scope())
	if len(none.calls) != 1 || !strings.Contains(none.calls[0], "bd sql --csv "+reaperOpRowCensusSQL) {
		t.Fatalf("calls = %q, want only the census", none.calls)
	}
	some := fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: "op"}
	_, _ = reaperOpRowsHeldBack(some.scope())
	want := []string{
		"/city: bd sql --csv " + reaperOpRowCensusSQL,
		"/city: bd purge --help",
		"/city: bd prune --help",
		"/city: bd config get --json types.retained",
	}
	if strings.Join(some.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", some.calls, want)
	}
}

func TestOpsRecordingDoctorCheckWarnsOnScopesTheReaperHoldsBack(t *testing.T) {
	held := fakeReaperScopeBd{census: "3"}
	clean := fakeReaperScopeBd{census: "0"}
	retained := fakeReaperScopeBd{census: "3", help: honorsRetainedTypesHelp, retained: "op"}
	unreachable := fakeReaperScopeBd{censusErr: errors.New("bd: store unreachable")}
	c, _ := newTestOpsRecordingCheck(newOpSweepStore(), opSweepT0)
	c.reaperScopes = func() []reaperOpRowsScope {
		return []reaperOpRowsScope{
			{label: "city", dir: "/city", run: held.run},
			{label: "rig:api", dir: "/api", run: clean.run},
			{label: "rig:web", dir: "/web", run: retained.run},
			// Unreachable: the bead-store checks report it, not this one.
			{label: "rig:ops", dir: "/ops", run: unreachable.run},
		}
	}
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q", r.Status, r.Message)
	}
	if !strings.HasPrefix(r.Message, "ops recording is off; ") || !strings.Contains(r.Message, "the core reaper does not purge or prune 1 scope(s) holding op rows") {
		t.Fatalf("message = %q", r.Message)
	}
	if len(r.Details) != 1 || r.Details[0] != "city: holds op rows and its bd does not honor types.retained" {
		t.Fatalf("details = %q", r.Details)
	}
	if !strings.Contains(r.FixHint, "gc bd config set types.retained op") {
		t.Fatalf("fix hint = %q", r.FixHint)
	}
}

func TestOpsRecordingDoctorCheckWithoutHeldScopesIsUnchanged(t *testing.T) {
	clean := fakeReaperScopeBd{census: "0"}
	c, _ := newTestOpsRecordingCheck(newOpSweepStore(), opSweepT0)
	c.reaperScopes = func() []reaperOpRowsScope {
		return []reaperOpRowsScope{{label: "city", dir: "/city", run: clean.run}}
	}
	if r := c.Run(&doctor.CheckContext{}); r.Status != doctor.StatusOK || r.Message != "ops recording is off" || len(r.Details) != 0 {
		t.Fatalf("result = %v %q %q, want OK, ops recording is off", r.Status, r.Message, r.Details)
	}
}

// TestOpsRecordingDoctorCheckKeepsAnErrorWhenTheReaperHoldsBack: a held
// scope adds its warning to an Error without lowering it.
func TestOpsRecordingDoctorCheckKeepsAnErrorWhenTheReaperHoldsBack(t *testing.T) {
	held := fakeReaperScopeBd{census: "1"}
	c, _ := newTestOpsRecordingCheck(nil, opSweepT0, rollout.WithOpsContent(rollout.Require))
	c.open = func() (beads.Store, error) { return nil, errors.New("store down") }
	c.reaperScopes = func() []reaperOpRowsScope {
		return []reaperOpRowsScope{{label: "city", dir: "/city", run: held.run}}
	}
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusError || !strings.Contains(r.Message, "the core reaper does not purge or prune 1 scope(s)") {
		t.Fatalf("result = %v %q, want the Error kept with the reaper warning added", r.Status, r.Message)
	}
}
