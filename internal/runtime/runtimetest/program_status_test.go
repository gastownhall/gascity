package runtimetest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// misreader is a provider whose program-status readings are its inner
// provider's, rewritten: a provider that breaks RUNTIME-CONTRACT-017.
type misreader struct {
	runtime.Provider
	rewrite func(runtime.ProgramStatus) runtime.ProgramStatus
}

func (m misreader) ProgramStatus(name string) (runtime.ProgramStatus, error) {
	ps, ok := runtime.ProgramStatusFor(m.Provider, name)
	if !ok {
		return runtime.ProgramStatus{}, errors.New("inner provider takes no reading")
	}
	r, err := ps.ProgramStatus(name)
	if err != nil {
		return r, err
	}
	return m.rewrite(r), nil
}

// misreading wraps every session the fake fixtures start in a misreader.
func misreading(rewrite func(runtime.ProgramStatus) runtime.ProgramStatus) ProgramStatusFixtures {
	f := FakeProgramStatusFixtures()
	wrap := func(s ProgramStatusSession) ProgramStatusSession {
		s.Provider = misreader{Provider: s.Provider, rewrite: rewrite}
		return s
	}
	armed, unarmed, inherited := f.Armed, f.Unarmed, f.Inherited
	f.Armed = func(t *testing.T, m runtime.ProgramStatusMode, script ...string) ProgramStatusSession {
		return wrap(armed(t, m, script...))
	}
	f.Unarmed = func(t *testing.T, m runtime.ProgramStatusMode, script ...string) ProgramStatusSession {
		return wrap(unarmed(t, m, script...))
	}
	f.Inherited = func(t *testing.T, m runtime.ProgramStatusMode) ProgramStatusSession {
		return wrap(inherited(t, m))
	}
	return f
}

// caseNamed returns the PS case whose name starts with code.
func caseNamed(t *testing.T, code string) programStatusCase {
	t.Helper()
	for _, c := range programStatusCases {
		if strings.HasPrefix(c.name, code+"_") {
			return c
		}
	}
	t.Fatalf("no case %s", code)
	return programStatusCase{}
}

// Kills a case list that drops, reorders or renumbers a PS row.
func TestProgramStatusCasesArePS001ThroughPS014(t *testing.T) {
	if len(programStatusCases) != 14 {
		t.Fatalf("%d cases, want 14", len(programStatusCases))
	}
	for i, c := range programStatusCases {
		want := fmt.Sprintf("PS-%03d_", i+1)
		if !strings.HasPrefix(c.name, want) {
			t.Errorf("case %d is %q, want prefix %q", i, c.name, want)
		}
	}
}

// Kills a suite that cannot tell a provider breaking the contract from one
// keeping it: each misreading below breaks exactly the case named.
func TestProgramStatusContractCatchesMisreadings(t *testing.T) {
	cases := []struct {
		code    string
		rewrite func(runtime.ProgramStatus) runtime.ProgramStatus
	}{
		{"PS-001", func(r runtime.ProgramStatus) runtime.ProgramStatus {
			if r.Validity == runtime.ProgramStatusUnread {
				r.Validity, r.State, r.Mode = runtime.ProgramStatusValid, runtime.ProgramWorking, runtime.ProgramStatusOn
			}
			return r
		}},
		{"PS-002", func(r runtime.ProgramStatus) runtime.ProgramStatus { r.Mode = runtime.ProgramStatusOn; return r }},
		{"PS-003", func(r runtime.ProgramStatus) runtime.ProgramStatus {
			if r.Validity == runtime.ProgramStatusSilent {
				r.Validity = runtime.ProgramStatusCleared
			}
			return r
		}},
		{"PS-004", func(r runtime.ProgramStatus) runtime.ProgramStatus { r.Probed = false; return r }},
		{"PS-005", func(r runtime.ProgramStatus) runtime.ProgramStatus {
			r.Nonce = "a1.%1"
			return r
		}},
		{"PS-006", func(r runtime.ProgramStatus) runtime.ProgramStatus {
			if r.Validity == runtime.ProgramStatusReaderLost {
				r.Validity, r.State = runtime.ProgramStatusValid, runtime.ProgramWorking
			}
			return r
		}},
		{"PS-007", func(r runtime.ProgramStatus) runtime.ProgramStatus { r.LastError = false; return r }},
		{"PS-008", func(r runtime.ProgramStatus) runtime.ProgramStatus {
			if r.Validity == runtime.ProgramStatusNotArmed {
				r.Validity, r.State = runtime.ProgramStatusValid, runtime.ProgramWorking
			}
			return r
		}},
		{"PS-009", func(r runtime.ProgramStatus) runtime.ProgramStatus {
			r.Since = r.Since.Add(-runtime.ProgramStatusHoldLimit)
			return r
		}},
		{"PS-010", func(r runtime.ProgramStatus) runtime.ProgramStatus {
			r.Kind = runtime.ProgramBlockUnspecified
			return r
		}},
		{"PS-011", func(r runtime.ProgramStatus) runtime.ProgramStatus { r.WorkingChildren = 0; return r }},
		{"PS-012", func(r runtime.ProgramStatus) runtime.ProgramStatus { r.Seq, r.TurnStarts = 0, 0; return r }},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			c := caseNamed(t, tc.code)
			if err := c.run(t, FakeProgramStatusFixtures()); err != nil {
				t.Fatalf("the fake breaks %s: %v", tc.code, err)
			}
			err := c.run(t, misreading(tc.rewrite))
			var s skipError
			if err == nil || errors.As(err, &s) {
				t.Fatalf("%s passed a provider that breaks it (err %v)", tc.code, err)
			}
		})
	}
}

// Kills heal cases that pass a healer taking a foreign pipe over or never
// arming, and a suite that runs heal cases without a healer.
func TestProgramStatusContractCatchesBadHeals(t *testing.T) {
	takeover := FakeProgramStatusFixtures()
	armed := takeover.Armed
	takeover.Armed = func(t *testing.T, m runtime.ProgramStatusMode, script ...string) ProgramStatusSession {
		s := armed(t, m, script...)
		s.Provider = alwaysArms{Provider: s.Provider}
		return s
	}
	if err := caseNamed(t, "PS-013").run(t, takeover); err == nil {
		t.Fatal("PS-013 passed a healer that takes a foreign pipe over")
	}
	stuck := FakeProgramStatusFixtures()
	unarmed := stuck.Unarmed
	stuck.Unarmed = func(t *testing.T, m runtime.ProgramStatusMode, script ...string) ProgramStatusSession {
		s := unarmed(t, m, script...)
		s.Provider = neverArms{Provider: s.Provider}
		return s
	}
	if err := caseNamed(t, "PS-014").run(t, stuck); err == nil {
		t.Fatal("PS-014 passed a healer that never arms")
	}
	noHealer := misreading(func(r runtime.ProgramStatus) runtime.ProgramStatus { return r })
	var s skipError
	for _, code := range []string{"PS-013", "PS-014"} {
		if err := caseNamed(t, code).run(t, noHealer); !errors.As(err, &s) {
			t.Fatalf("%s without a healer = %v, want a skip", code, err)
		}
	}
}

// alwaysArms heals by reporting armed whatever the pane holds.
type alwaysArms struct{ runtime.Provider }

func (a alwaysArms) ProgramStatus(name string) (runtime.ProgramStatus, error) {
	ps, _ := runtime.ProgramStatusFor(a.Provider, name)
	return ps.ProgramStatus(name)
}

func (alwaysArms) HealProgramCapture(string, runtime.ProgramStatusArm) (runtime.ProgramCaptureAction, error) {
	return runtime.ProgramCaptureArmed, nil
}

// neverArms heals by reporting nothing to do.
type neverArms struct{ runtime.Provider }

func (n neverArms) ProgramStatus(name string) (runtime.ProgramStatus, error) {
	ps, _ := runtime.ProgramStatusFor(n.Provider, name)
	return ps.ProgramStatus(name)
}

func (neverArms) HealProgramCapture(string, runtime.ProgramStatusArm) (runtime.ProgramCaptureAction, error) {
	return runtime.ProgramCaptureUnchanged, nil
}

// Kills a runner that fails, rather than skips, a case its fixtures cannot
// build.
func TestRunProgramStatusContractSkipsWithoutFixtures(t *testing.T) {
	RunProgramStatusContract(t, ProgramStatusFixtures{})
}
