package tmux

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

type fakeProcessStartTime struct {
	value string
	err   error
}

func (s *fakeProcessStartTime) read(int) (string, error) {
	return s.value, s.err
}

func TestCapturePaneProcessKillPlanBindsStableLivePaneToSnapshot(t *testing.T) {
	const panePID = 100
	live := paneProcessState{PID: panePID}
	records := []proctable.ProcessRecord{
		{PID: panePID, PPID: 1, PGID: panePID, StartTime: "pane-start"},
		{PID: 101, PPID: panePID, PGID: panePID, StartTime: "child-start"},
	}

	t.Run("tmux observes PID and dead state atomically", func(t *testing.T) {
		executor := &fakeExecutor{out: "100\t0\n"}
		tmux := NewTmux()
		tmux.exec = executor
		state, err := tmux.paneProcessState("session")
		if err != nil {
			t.Fatalf("paneProcessState: %v", err)
		}
		if state != live {
			t.Fatalf("pane process state = %+v, want %+v", state, live)
		}
		if len(executor.calls) != 1 {
			t.Fatalf("tmux calls = %d, want one atomic observation", len(executor.calls))
		}
		want := []string{"-u", "display-message", "-t", "session:^.0", "-p", "#{pane_pid}\t#{pane_dead}"}
		if !slices.Equal(executor.calls[0], want) {
			t.Fatalf("tmux args = %q, want atomic pid/dead format %q", executor.calls[0], want)
		}
	})

	t.Run("malformed atomic pane output is rejected", func(t *testing.T) {
		for _, output := range []string{"", "100", "100 0 extra", "not-a-pid 0", "1 0", "100 maybe"} {
			state, err := parsePaneProcessState(output, "session")
			if err == nil {
				t.Errorf("parsePaneProcessState(%q) = %+v, nil; want error", output, state)
			}
			if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer) {
				t.Errorf("parsePaneProcessState(%q) error = %v; malformed output must not claim target disappearance", output, err)
			}
		}
	})

	t.Run("pane API surfaces first observation disappearance", func(t *testing.T) {
		for _, observationErr := range []error{ErrSessionNotFound, ErrNoServer} {
			executor := &fakeExecutor{err: observationErr}
			tmux := NewTmux()
			tmux.exec = executor
			err := tmux.KillPaneProcesses("%7")
			if !errors.Is(err, observationErr) {
				t.Fatalf("KillPaneProcesses first observation error = %v, want %v", err, observationErr)
			}
			if len(executor.calls) != 1 {
				t.Fatalf("tmux calls = %d, want only the failed first observation", len(executor.calls))
			}
		}
	})

	t.Run("empty successful output needs session-owner corroboration", func(t *testing.T) {
		t.Run("pane remains indeterminate", func(t *testing.T) {
			executor := &fakeExecutor{out: "\t\n"}
			tmux := NewTmux()
			tmux.exec = executor
			err := tmux.KillPaneProcesses("%7")
			if err == nil {
				t.Fatal("KillPaneProcesses empty successful observation = nil, want indeterminate error")
			}
			if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer) {
				t.Fatalf("KillPaneProcesses empty successful observation = %v, must not claim disappearance", err)
			}
		})

		t.Run("missing session owner corroborates absence", func(t *testing.T) {
			executor := &fakeExecutor{
				outs: []string{"\t\n", ""},
				errs: []error{nil, ErrSessionNotFound},
			}
			tmux := NewTmux()
			tmux.exec = executor
			if err := tmux.KillSessionWithProcesses("session"); err != nil {
				t.Fatalf("KillSessionWithProcesses repeated stop = %v, want corroborated absence", err)
			}
			if len(executor.calls) != 2 {
				t.Fatalf("tmux calls = %d, want observation then owner kill", len(executor.calls))
			}
		})

		t.Run("successful session owner does not erase indeterminate discovery", func(t *testing.T) {
			executor := &fakeExecutor{outs: []string{"\t\n", ""}}
			tmux := NewTmux()
			tmux.exec = executor
			err := tmux.KillSessionWithProcesses("session")
			if err == nil {
				t.Fatal("KillSessionWithProcesses empty discovery plus successful owner kill = nil, want discovery error")
			}
			if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer) {
				t.Fatalf("KillSessionWithProcesses error = %v, malformed discovery must remain distinct from absence", err)
			}
		})
	})

	t.Run("pane cleanup never signals a dead pane stale PID", func(t *testing.T) {
		executor := &fakeExecutor{out: "100\t1\n"}
		tmux := NewTmux()
		tmux.exec = executor
		if err := tmux.KillPaneProcesses("%7"); err != nil {
			t.Fatalf("KillPaneProcesses(dead pane): %v", err)
		}
		if len(executor.calls) != 1 {
			t.Fatalf("tmux calls = %d, want only the dead-state observation", len(executor.calls))
		}
	})

	t.Run("stable live pane builds plan", func(t *testing.T) {
		observations := []paneProcessState{live, live}
		identities := []string{"pane-start", "pane-start"}
		var calls []string
		plan, err := capturePaneProcessKillPlan(
			func() (paneProcessState, error) {
				calls = append(calls, "observe")
				observation := observations[0]
				observations = observations[1:]
				return observation, nil
			},
			func() ([]proctable.ProcessRecord, error) {
				calls = append(calls, "snapshot")
				return records, nil
			},
			func(pid int) (string, error) {
				calls = append(calls, "identity")
				if pid != panePID {
					t.Fatalf("identity PID = %d, want %d", pid, panePID)
				}
				identity := identities[0]
				identities = identities[1:]
				return identity, nil
			},
			nil,
		)
		if err != nil {
			t.Fatalf("capture plan: %v", err)
		}
		if want := []string{"observe", "identity", "snapshot", "observe", "identity"}; !slices.Equal(calls, want) {
			t.Fatalf("calls = %v, want strict observation/identity bracket %v", calls, want)
		}
		if plan.Leader == nil || *plan.Leader != (processTarget{PID: panePID, StartTime: "pane-start"}) {
			t.Fatalf("leader = %v, want bound pane identity", plan.Leader)
		}
		if want := []processTarget{{PID: 101, StartTime: "child-start"}}; !slices.Equal(plan.Descendants, want) {
			t.Fatalf("descendants = %v, want %v", plan.Descendants, want)
		}
	})

	t.Run("dead stale pane never binds replacement from snapshot", func(t *testing.T) {
		snapshotCalled := false
		plan, err := capturePaneProcessKillPlan(
			func() (paneProcessState, error) { return paneProcessState{PID: panePID, Dead: true}, nil },
			func() ([]proctable.ProcessRecord, error) {
				snapshotCalled = true
				return []proctable.ProcessRecord{{PID: panePID, StartTime: "foreign"}}, nil
			},
			func(int) (string, error) { t.Fatal("dead pane identity was read"); return "", nil },
			nil,
		)
		if err != nil {
			t.Fatalf("dead pane capture: %v", err)
		}
		if snapshotCalled || plan.Leader != nil || len(plan.Descendants) != 0 {
			t.Fatalf("dead pane capture = %+v, snapshotCalled=%t; want empty fail-closed plan", plan, snapshotCalled)
		}
	})

	for _, test := range []struct {
		name         string
		observations []paneProcessState
		identities   []string
		identityErrs []error
	}{
		{
			name:         "root disappears before snapshot",
			observations: []paneProcessState{live},
			identityErrs: []error{proctable.ErrProcessGone},
		},
		{
			name:         "pane dies after snapshot",
			observations: []paneProcessState{live, {PID: panePID, Dead: true}},
			identities:   []string{"pane-start"},
		},
		{
			name:         "root disappears after snapshot",
			observations: []paneProcessState{live, live},
			identities:   []string{"pane-start"},
			identityErrs: []error{nil, proctable.ErrProcessGone},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			observations := slices.Clone(test.observations)
			identities := slices.Clone(test.identities)
			identityErrs := slices.Clone(test.identityErrs)
			plan, err := capturePaneProcessKillPlan(
				func() (paneProcessState, error) {
					observation := observations[0]
					observations = observations[1:]
					return observation, nil
				},
				func() ([]proctable.ProcessRecord, error) { return records, nil },
				func(int) (string, error) {
					var identityErr error
					if len(identityErrs) > 0 {
						identityErr = identityErrs[0]
						identityErrs = identityErrs[1:]
					}
					if identityErr != nil {
						return "", identityErr
					}
					identity := identities[0]
					identities = identities[1:]
					return identity, nil
				},
				map[int]bool{102: true},
			)
			if err != nil {
				t.Fatalf("benign disappearance returned error: %v", err)
			}
			if plan.Leader != nil || len(plan.Descendants) != 0 || !plan.PreserveExclusion {
				t.Fatalf("benign disappearance plan = %+v, want empty with conservative exclusion ordering", plan)
			}
		})
	}

	t.Run("second pane observation disappearance is benign", func(t *testing.T) {
		for _, observationErr := range []error{ErrSessionNotFound, ErrNoServer} {
			observations := 0
			plan, err := capturePaneProcessKillPlan(
				func() (paneProcessState, error) {
					observations++
					if observations == 2 {
						return paneProcessState{}, observationErr
					}
					return live, nil
				},
				func() ([]proctable.ProcessRecord, error) { return records, nil },
				func(int) (string, error) { return "pane-start", nil },
				map[int]bool{102: true},
			)
			if err != nil {
				t.Fatalf("second observation %v returned error: %v", observationErr, err)
			}
			if plan.Leader != nil || len(plan.Descendants) != 0 || !plan.PreserveExclusion {
				t.Fatalf("second observation disappearance plan = %+v, want empty with conservative exclusion ordering", plan)
			}
		}
	})

	for _, test := range []struct {
		name              string
		observations      []paneProcessState
		observationErrors []error
		identities        []string
		identityErrors    []error
		records           []proctable.ProcessRecord
		snapshotError     error
	}{
		{
			name:              "first pane observation disappears",
			observationErrors: []error{ErrSessionNotFound},
		},
		{
			name:              "first pane observation finds no server",
			observationErrors: []error{ErrNoServer},
		},
		{
			name:              "first pane observation times out",
			observationErrors: []error{context.DeadlineExceeded},
		},
		{
			name:              "second pane observation times out",
			observations:      []paneProcessState{live},
			observationErrors: []error{nil, context.DeadlineExceeded},
			identities:        []string{"pane-start"},
			records:           records,
		},
		{
			name:           "first root identity read fails",
			observations:   []paneProcessState{live},
			identityErrors: []error{errors.New("identity unreadable")},
		},
		{
			name:           "second root identity read fails",
			observations:   []paneProcessState{live, live},
			identities:     []string{"pane-start"},
			identityErrors: []error{nil, errors.New("identity unreadable")},
			records:        records,
		},
		{
			name:         "pane PID changes across snapshot",
			observations: []paneProcessState{live, {PID: 200}},
			identities:   []string{"pane-start"},
			records:      records,
		},
		{
			name:         "pane root identity changes across snapshot",
			observations: []paneProcessState{live, live},
			identities:   []string{"pane-start", "reused-start"},
			records:      records,
		},
		{
			name:         "snapshot root belongs to predecessor",
			observations: []paneProcessState{live, live},
			identities:   []string{"pane-start", "pane-start"},
			records: []proctable.ProcessRecord{
				{PID: panePID, PPID: 1, PGID: panePID, StartTime: "predecessor-start"},
				{PID: 101, PPID: panePID, PGID: panePID, StartTime: "predecessor-child"},
			},
		},
		{
			name:          "snapshot failure",
			observations:  []paneProcessState{live},
			identities:    []string{"pane-start"},
			snapshotError: errors.New("snapshot unavailable"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			observations := slices.Clone(test.observations)
			observationErrors := slices.Clone(test.observationErrors)
			identities := slices.Clone(test.identities)
			identityErrors := slices.Clone(test.identityErrors)
			plan, err := capturePaneProcessKillPlan(
				func() (paneProcessState, error) {
					var observationErr error
					if len(observationErrors) > 0 {
						observationErr = observationErrors[0]
						observationErrors = observationErrors[1:]
					}
					if observationErr != nil {
						return paneProcessState{}, observationErr
					}
					observation := observations[0]
					observations = observations[1:]
					return observation, nil
				},
				func() ([]proctable.ProcessRecord, error) { return test.records, test.snapshotError },
				func(int) (string, error) {
					var identityErr error
					if len(identityErrors) > 0 {
						identityErr = identityErrors[0]
						identityErrors = identityErrors[1:]
					}
					if identityErr != nil {
						return "", identityErr
					}
					identity := identities[0]
					identities = identities[1:]
					return identity, nil
				},
				map[int]bool{102: true},
			)
			if err == nil {
				t.Fatal("unsafe pane binding returned nil error")
			}
			if plan.Leader != nil || len(plan.Descendants) != 0 || !plan.PreserveExclusion {
				t.Fatalf("unsafe pane binding plan = %+v, want empty with conservative exclusion ordering", plan)
			}
		})
	}
}

func TestKillSessionProcessTeardownOwnerFirstWithoutOwnedExclusion(t *testing.T) {
	leader := processTarget{PID: 100, StartTime: "leader"}
	plan := processKillPlan{
		Descendants: []processTarget{{PID: 101, StartTime: "child"}},
		Leader:      &leader,
	}
	wantErr := errors.New("kill-session failed")
	var calls []string

	err := teardownSessionProcessPlan(
		plan,
		nil,
		func() error { calls = append(calls, "kill-session"); return wantErr },
		func(processKillPlan) error { calls = append(calls, "terminate-fallback"); return nil },
	)

	if !errors.Is(err, wantErr) {
		t.Fatalf("teardown error = %v, want %v", err, wantErr)
	}
	if want := []string{"kill-session", "terminate-fallback"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want tmux owner before identity-fenced fallback %v", calls, want)
	}

	t.Run("real discovery and termination errors survive missing-session normalization", func(t *testing.T) {
		discoveryErr := context.DeadlineExceeded
		terminationErr := errors.New("signal denied")
		err := teardownSessionProcessPlan(
			plan,
			discoveryErr,
			func() error { return ErrSessionNotFound },
			func(processKillPlan) error { return terminationErr },
		)
		if !errors.Is(err, discoveryErr) || !errors.Is(err, terminationErr) {
			t.Fatalf("teardown error = %v, want discovery %v and termination %v", err, discoveryErr, terminationErr)
		}
		if errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("normalized kill-session error leaked into joined result: %v", err)
		}
	})

	t.Run("observation disappearance is benign after owner teardown", func(t *testing.T) {
		err := teardownSessionProcessPlan(
			processKillPlan{},
			fmt.Errorf("observing pane: %w", ErrSessionNotFound),
			func() error { return ErrSessionNotFound },
			func(processKillPlan) error { return nil },
		)
		if err != nil {
			t.Fatalf("already-gone session teardown error = %v, want nil", err)
		}
	})
}

func TestKillSessionProcessTeardownOwnedExclusionKeepsCleanupFirst(t *testing.T) {
	const callerPID = 102
	plan := buildProcessKillPlan(100, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, PGID: 100, StartTime: "leader"},
		{PID: 101, PPID: 100, PGID: 100, StartTime: "cleanup"},
		{PID: callerPID, PPID: 100, PGID: 100, StartTime: "caller"},
	}, map[int]bool{callerPID: true})
	var calls []string

	err := teardownSessionProcessPlan(
		plan,
		nil,
		func() error { calls = append(calls, "kill-session"); return nil },
		func(processKillPlan) error { calls = append(calls, "terminate-non-excluded"); return nil },
	)
	if err != nil {
		t.Fatalf("teardown error: %v", err)
	}
	if !plan.PreserveExclusion {
		t.Fatal("captured caller exclusion did not preserve cleanup-first ordering")
	}
	if want := []string{"terminate-non-excluded", "kill-session"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want owned exclusion cleanup before session teardown %v", calls, want)
	}

	t.Run("failed discovery conservatively keeps exclusion cleanup first", func(t *testing.T) {
		discoveryErr := errors.New("snapshot unavailable")
		var calls []string
		err := teardownSessionProcessPlan(
			processKillPlan{PreserveExclusion: true},
			discoveryErr,
			func() error { calls = append(calls, "kill-session"); return nil },
			func(processKillPlan) error { calls = append(calls, "terminate-empty"); return nil },
		)
		if !errors.Is(err, discoveryErr) {
			t.Fatalf("teardown error = %v, want discovery error %v", err, discoveryErr)
		}
		if want := []string{"terminate-empty", "kill-session"}; !slices.Equal(calls, want) {
			t.Fatalf("calls = %v, want conservative exclusion order %v", calls, want)
		}
	})
}

func TestProviderEnvSkipsEscapeForPiAlias(t *testing.T) {
	if !providerEnvSkipsEscape("my-pi/tmux") {
		t.Fatal("pi provider alias should skip pre-enter Escape")
	}
}

func TestProviderEnvSkipsEscapeForCopilot(t *testing.T) {
	if !providerEnvSkipsEscape("copilot") {
		t.Fatal("copilot provider should skip pre-enter Escape")
	}
}

// TestComputeExcludingKillSet_SelfCloseExcludesCallerKeepsAgent locks in the
// fix for the self-close wedge: when `gc session close` runs from inside the
// pane it is tearing down, the caller is a descendant of the pane leader (the
// agent). The caller must be excluded from the TERM list so it survives long
// enough to finish cleanup, while the pane leader (agent) is still reached.
func TestComputeExcludingKillSet_SelfCloseExcludesCallerKeepsAgent(t *testing.T) {
	const (
		agentPID  = 100 // pane leader (e.g. the coding agent) — must be killed
		shellPID  = 101 // intermediate shell spawned by the agent
		callerPID = 102 // gc session close — the excluded caller
	)
	records := []proctable.ProcessRecord{
		{PID: agentPID, PPID: 1, PGID: agentPID, StartTime: "agent-start"},
		{PID: shellPID, PPID: agentPID, PGID: agentPID, StartTime: "shell-start"},
		{PID: callerPID, PPID: shellPID, PGID: agentPID, StartTime: "caller-start"},
	}

	plan := buildProcessKillPlan(agentPID, records, map[int]bool{callerPID: true})

	if !plan.PreserveExclusion {
		t.Fatal("owned caller exclusion must keep session teardown after direct cleanup")
	}
	if plan.Leader == nil || plan.Leader.PID != agentPID {
		t.Error("pane leader (agent) must be killed, but it was reported excluded")
	}
	if slices.ContainsFunc(plan.Descendants, func(target processTarget) bool { return target.PID == callerPID }) {
		t.Errorf("caller %d must be excluded from TERM list, got %v", callerPID, plan.Descendants)
	}
	if !slices.ContainsFunc(plan.Descendants, func(target processTarget) bool { return target.PID == shellPID }) {
		t.Errorf("non-excluded descendant %d must be in TERM list, got %v", shellPID, plan.Descendants)
	}
}

// TestComputeExcludingKillSet_ExternalCallerKillsEverything verifies that when
// the caller lives outside the pane (e.g. the supervisor running the close),
// excluding its PID is a harmless no-op: every process in the pane's tree is
// still terminated.
func TestComputeExcludingKillSet_ExternalCallerKillsEverything(t *testing.T) {
	const agentPID = 200
	records := []proctable.ProcessRecord{
		// The root points back to its grandchild to plant the PID-reuse cycle.
		{PID: agentPID, PPID: 202, PGID: agentPID, StartTime: "agent"},
		{PID: 101, PPID: 1, PGID: 101, StartTime: "foreign-gui"},
		{PID: 202, PPID: 201, PGID: agentPID, StartTime: "grandchild"},
		{PID: 204, PPID: 1, PGID: agentPID, StartTime: "init-orphan"},
		{PID: 201, PPID: agentPID, PGID: agentPID, StartTime: "child"},
		{PID: 205, PPID: 900, PGID: agentPID, StartTime: "subreaper-orphan"},
		{PID: 300, PPID: 1, PGID: 300, StartTime: "sibling-pane"},
	}

	plan := buildProcessKillPlan(agentPID, records, map[int]bool{999: true})
	if plan.PreserveExclusion {
		t.Fatal("foreign exclusion must not delay tmux-owned session teardown")
	}
	want := []processTarget{
		{PID: 202, StartTime: "grandchild"},
		{PID: 201, StartTime: "child"},
		{PID: 204, StartTime: "init-orphan"},
		{PID: 205, StartTime: "subreaper-orphan"},
	}
	if !slices.Equal(plan.Descendants, want) {
		t.Fatalf("descendants = %v, want deterministic deepest-first targets %v", plan.Descendants, want)
	}
	if plan.Leader == nil || *plan.Leader != (processTarget{PID: agentPID, StartTime: "agent"}) {
		t.Fatalf("leader = %v, want pane identity", plan.Leader)
	}
	for _, foreign := range []int{101, 300} {
		if slices.ContainsFunc(plan.Descendants, func(target processTarget) bool { return target.PID == foreign }) {
			t.Errorf("foreign PID %d entered kill plan %v", foreign, plan.Descendants)
		}
	}

	// Input order is not process order. Shuffling the same snapshot must not
	// change signal order.
	slices.Reverse(records)
	shuffled := buildProcessKillPlan(agentPID, records, map[int]bool{999: true})
	if !slices.Equal(shuffled.Descendants, plan.Descendants) {
		t.Fatalf("shuffled descendants = %v, want %v", shuffled.Descendants, plan.Descendants)
	}

	t.Run("ambiguous root identity fails closed", func(t *testing.T) {
		duplicate := append(slices.Clone(records), proctable.ProcessRecord{
			PID: agentPID, PPID: 1, PGID: agentPID, StartTime: "other-agent",
		})
		got := buildProcessKillPlan(agentPID, duplicate, nil)
		if got.Leader != nil || len(got.Descendants) != 0 {
			t.Fatalf("duplicate-PID snapshot plan = %+v, want empty", got)
		}

		missingRootIdentity := slices.Clone(records)
		for index := range missingRootIdentity {
			if missingRootIdentity[index].PID == agentPID {
				missingRootIdentity[index].StartTime = ""
			}
		}
		got = buildProcessKillPlan(agentPID, missingRootIdentity, nil)
		if got.Leader != nil || len(got.Descendants) != 0 {
			t.Fatalf("missing-root-identity plan = %+v, want empty", got)
		}

		got = buildProcessKillPlan(agentPID, duplicate, map[int]bool{agentPID: true})
		if !got.PreserveExclusion {
			t.Fatal("ambiguous snapshot must conservatively preserve a possible owned exclusion")
		}
	})
}

// TestComputeExcludingKillSet_ExcludedPaneLeaderSurvives guards the degenerate
// case where the pane leader itself is in the exclusion set: it must not be
// signaled directly (the final tmux kill-session reaps it instead).
func TestComputeExcludingKillSet_ExcludedPaneLeaderSurvives(t *testing.T) {
	const agentPID = 300
	records := []proctable.ProcessRecord{
		{PID: agentPID, PPID: 1, PGID: agentPID, StartTime: "agent"},
		{PID: 301, PPID: agentPID, PGID: agentPID, StartTime: "child"},
	}

	plan := buildProcessKillPlan(agentPID, records, map[int]bool{agentPID: true})

	if !plan.PreserveExclusion {
		t.Fatal("excluded pane leader must keep session teardown after direct cleanup")
	}
	if plan.Leader != nil {
		t.Error("an excluded pane leader must not be killed directly")
	}
	if want := []processTarget{{PID: 301, StartTime: "child"}}; !slices.Equal(plan.Descendants, want) {
		t.Fatalf("descendants = %v, want %v", plan.Descendants, want)
	}
}

func TestTerminateProcessSetReturnsWhenTerminatedProcessesExit(t *testing.T) {
	targets := []processTarget{{PID: 101, StartTime: "a"}, {PID: 102, StartTime: "b"}}
	alive := map[int]bool{101: true, 102: true}
	var signals []string
	var sleeps []time.Duration
	now := time.Unix(0, 0)

	if err := terminateProcessSet(
		targets,
		time.Second,
		func(target processTarget, signal processSignal) (bool, error) {
			signals = append(signals, string(signal)+":"+target.String())
			if signal == processSignalTerm {
				alive[target.PID] = false
			}
			return true, nil
		},
		func(target processTarget) (bool, error) { return alive[target.PID], nil },
		func(delay time.Duration) {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
		},
		func() time.Time { return now },
	); err != nil {
		t.Fatalf("terminateProcessSet: %v", err)
	}

	if want := []string{"TERM:101", "TERM:102"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleep calls = %v, want none after TERM made every process exit", sleeps)
	}
}

func TestTerminateProcessSetKillsOnlyProcessesStillAliveAfterGracePeriod(t *testing.T) {
	targets := []processTarget{{PID: 201, StartTime: "a"}, {PID: 202, StartTime: "b"}}
	alive := map[int]bool{201: true, 202: true}
	var signals []string
	var slept time.Duration
	now := time.Unix(0, 0)

	if err := terminateProcessSet(
		targets,
		2*processExitCheckInterval,
		func(target processTarget, signal processSignal) (bool, error) {
			signals = append(signals, string(signal)+":"+target.String())
			if signal == processSignalTerm && target.PID == 201 {
				alive[target.PID] = false
			}
			return true, nil
		},
		func(target processTarget) (bool, error) { return alive[target.PID], nil },
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	); err != nil {
		t.Fatalf("terminateProcessSet: %v", err)
	}

	want := []string{"TERM:201", "TERM:202", "KILL:202"}
	if !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != 2*processExitCheckInterval {
		t.Fatalf("slept = %s, want full grace period %s for surviving process", slept, 2*processExitCheckInterval)
	}
}

func TestTerminateProcessSetReturnsWhenProcessExitsDuringGracePeriod(t *testing.T) {
	var signals []string
	checks := 0
	slept := time.Duration(0)
	now := time.Unix(0, 0)

	if err := terminateProcessSet(
		[]processTarget{{PID: 301, StartTime: "a"}},
		time.Second,
		func(target processTarget, signal processSignal) (bool, error) {
			signals = append(signals, string(signal)+":"+target.String())
			return true, nil
		},
		func(processTarget) (bool, error) {
			checks++
			return checks < 3, nil
		},
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	); err != nil {
		t.Fatalf("terminateProcessSet: %v", err)
	}

	if want := []string{"TERM:301"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != 2*processExitCheckInterval {
		t.Fatalf("slept = %s, want two observations (%s)", slept, 2*processExitCheckInterval)
	}
}

func TestTerminateProcessSetCountsProbeTimeAgainstGracePeriod(t *testing.T) {
	var signals []string
	slept := time.Duration(0)
	now := time.Unix(0, 0)
	probeDuration := 2 * processExitCheckInterval

	if err := terminateProcessSet(
		[]processTarget{{PID: 401, StartTime: "a"}},
		3*processExitCheckInterval,
		func(target processTarget, signal processSignal) (bool, error) {
			signals = append(signals, string(signal)+":"+target.String())
			return true, nil
		},
		func(processTarget) (bool, error) {
			now = now.Add(probeDuration)
			return true, nil
		},
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	); err != nil {
		t.Fatalf("terminateProcessSet: %v", err)
	}

	if want := []string{"TERM:401", "KILL:401"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != processExitCheckInterval {
		t.Fatalf("slept = %s, want remaining grace budget %s after slow probe", slept, processExitCheckInterval)
	}

	t.Run("identity mismatch before TERM signals nothing and returns immediately", func(t *testing.T) {
		target := processTarget{PID: 501, StartTime: "original"}
		var signaled []processSignal
		var slept time.Duration
		currentStart := (&fakeProcessStartTime{value: "recycled"}).read

		if err := terminateProcessSet(
			[]processTarget{target},
			time.Second,
			func(target processTarget, signal processSignal) (bool, error) {
				return signalProcessTargetIfCurrent(target, signal, currentStart, func(_ int, signal processSignal) error {
					signaled = append(signaled, signal)
					return nil
				})
			},
			func(processTarget) (bool, error) { return true, nil },
			func(delay time.Duration) { slept += delay },
			time.Now,
		); err != nil {
			t.Fatalf("terminateProcessSet: %v", err)
		}

		if len(signaled) != 0 {
			t.Fatalf("signals = %v, want none for recycled PID", signaled)
		}
		if slept != 0 {
			t.Fatalf("slept = %s, want immediate return for refused TERM", slept)
		}
	})

	t.Run("identity change during grace removes target before KILL", func(t *testing.T) {
		target := processTarget{PID: 502, StartTime: "original"}
		var signaled []processSignal
		var slept time.Duration
		now := time.Unix(0, 0)
		startTime := &fakeProcessStartTime{value: "original"}

		if err := terminateProcessSet(
			[]processTarget{target},
			2*processExitCheckInterval,
			func(target processTarget, signal processSignal) (bool, error) {
				return signalProcessTargetIfCurrent(target, signal, startTime.read, func(_ int, signal processSignal) error {
					signaled = append(signaled, signal)
					return nil
				})
			},
			func(target processTarget) (bool, error) { return verifyProcessTargetIdentity(target, startTime.read) },
			func(delay time.Duration) {
				slept += delay
				now = now.Add(delay)
				startTime.value = "recycled"
			},
			func() time.Time { return now },
		); err != nil {
			t.Fatalf("terminateProcessSet: %v", err)
		}

		if want := []processSignal{processSignalTerm}; !slices.Equal(signaled, want) {
			t.Fatalf("signals = %v, want %v; recycled target must not receive KILL", signaled, want)
		}
		if slept != processExitCheckInterval {
			t.Fatalf("slept = %s, want one observation interval before recycled identity exited the grace loop", slept)
		}
	})

	t.Run("unreadable identity fails closed", func(t *testing.T) {
		target := processTarget{PID: 503, StartTime: "original"}
		called := false
		signaled, err := signalProcessTargetIfCurrent(
			target,
			processSignalTerm,
			(&fakeProcessStartTime{err: errors.New("unreadable")}).read,
			func(int, processSignal) error { called = true; return nil },
		)
		if signaled {
			t.Fatal("signalProcessTargetIfCurrent = true for unreadable identity")
		}
		if err == nil {
			t.Fatal("unreadable identity error was swallowed")
		}
		if called {
			t.Fatal("signal syscall called for unreadable identity")
		}
	})

	t.Run("gone identity is a benign stale target", func(t *testing.T) {
		called := false
		signaled, err := signalProcessTargetIfCurrent(
			processTarget{PID: 503, StartTime: "original"},
			processSignalTerm,
			func(int) (string, error) { return "", proctable.ErrProcessGone },
			func(int, processSignal) error { called = true; return nil },
		)
		if err != nil || signaled || called {
			t.Fatalf("gone target result = (signaled=%t, err=%v, syscall=%t), want benign no-signal", signaled, err, called)
		}
	})

	t.Run("signal syscall failure does not enter grace", func(t *testing.T) {
		target := processTarget{PID: 504, StartTime: "original"}
		var slept time.Duration
		wantErr := errors.New("permission denied")
		err := terminateProcessSet(
			[]processTarget{target},
			time.Second,
			func(target processTarget, signal processSignal) (bool, error) {
				return signalProcessTargetIfCurrent(
					target,
					signal,
					(&fakeProcessStartTime{value: "original"}).read,
					func(int, processSignal) error { return wantErr },
				)
			},
			func(processTarget) (bool, error) { return true, nil },
			func(delay time.Duration) { slept += delay },
			time.Now,
		)
		if !errors.Is(err, wantErr) {
			t.Fatalf("terminateProcessSet error = %v, want signal failure %v", err, wantErr)
		}
		if slept != 0 {
			t.Fatalf("slept = %s after refused signal, want immediate return", slept)
		}
	})

	t.Run("signal error does not stop later targets", func(t *testing.T) {
		wantErr := errors.New("permission denied")
		var attempts []int
		err := terminateProcessSet(
			[]processTarget{{PID: 505, StartTime: "a"}, {PID: 506, StartTime: "b"}},
			time.Second,
			func(target processTarget, _ processSignal) (bool, error) {
				attempts = append(attempts, target.PID)
				if target.PID == 505 {
					return false, wantErr
				}
				return true, nil
			},
			func(processTarget) (bool, error) { return false, nil },
			func(time.Duration) {},
			time.Now,
		)
		if !errors.Is(err, wantErr) {
			t.Fatalf("terminateProcessSet error = %v, want %v", err, wantErr)
		}
		if want := []int{505, 506}; !slices.Equal(attempts, want) {
			t.Fatalf("TERM attempts = %v, want remaining target attempted %v", attempts, want)
		}
	})

	t.Run("indeterminate grace probe reports error and never sends KILL", func(t *testing.T) {
		wantErr := errors.New("identity unreadable")
		var signals []processSignal
		err := terminateProcessSet(
			[]processTarget{{PID: 507, StartTime: "a"}},
			processExitCheckInterval,
			func(_ processTarget, signal processSignal) (bool, error) {
				signals = append(signals, signal)
				return true, nil
			},
			func(processTarget) (bool, error) { return false, wantErr },
			func(time.Duration) {},
			time.Now,
		)
		if !errors.Is(err, wantErr) {
			t.Fatalf("terminateProcessSet error = %v, want %v", err, wantErr)
		}
		if want := []processSignal{processSignalTerm}; !slices.Equal(signals, want) {
			t.Fatalf("signals = %v, want TERM only after indeterminate probe %v", signals, want)
		}
	})
}

func TestReparentedOrphans_CollectsInitAndSubreaperOrphans(t *testing.T) {
	records := []proctable.ProcessRecord{
		{PID: 100, PPID: 1, PGID: 100, StartTime: "leader"},
		{PID: 200, PPID: 100, PGID: 100, StartTime: "descendant"},
		{PID: 300, PPID: 1, PGID: 100, StartTime: "init-orphan"},
		{PID: 400, PPID: 900, PGID: 100, StartTime: "subreaper-orphan"},
		{PID: 500, PPID: 200, PGID: 100, StartTime: "deep-descendant"},
		{PID: 600, PPID: 0, PGID: 100, StartTime: "unknown-parent"},
	}

	plan := buildProcessKillPlan(100, records, nil)
	for _, pid := range []int{300, 400} {
		if !slices.ContainsFunc(plan.Descendants, func(target processTarget) bool { return target.PID == pid }) {
			t.Errorf("same-PGID orphan %d missing from %v", pid, plan.Descendants)
		}
	}
	if slices.ContainsFunc(plan.Descendants, func(target processTarget) bool { return target.PID == 600 }) {
		t.Errorf("unknown-parent member entered kill plan %v", plan.Descendants)
	}

	t.Run("does not claim an inherited process group", verifyInheritedProcessGroupIsTreeOnly)
}

func verifyInheritedProcessGroupIsTreeOnly(t *testing.T) {
	records := []proctable.ProcessRecord{
		// The pane root inherited group 900 rather than leading its own group.
		// A foreign member of 900 is therefore not evidence of pane ownership.
		{PID: 100, PPID: 1, PGID: 900, StartTime: "leader"},
		{PID: 200, PPID: 100, PGID: 900, StartTime: "descendant"},
		{PID: 300, PPID: 1, PGID: 900, StartTime: "foreign-same-group"},
	}

	plan := buildProcessKillPlan(100, records, nil)
	want := []processTarget{{PID: 200, StartTime: "descendant"}}
	if !slices.Equal(plan.Descendants, want) {
		t.Fatalf("descendants = %v, want tree-only plan %v for inherited PGID", plan.Descendants, want)
	}
}

func TestReparentedOrphans_SkipsKnownDescendants(t *testing.T) {
	records := []proctable.ProcessRecord{
		{PID: 100, PPID: 1, PGID: 100, StartTime: "leader"},
		{PID: 200, PPID: 100, PGID: 100, StartTime: "child-a"},
		{PID: 300, PPID: 100, PGID: 100, StartTime: "child-b"},
	}
	plan := buildProcessKillPlan(100, records, nil)
	want := []processTarget{{PID: 200, StartTime: "child-a"}, {PID: 300, StartTime: "child-b"}}
	if !slices.Equal(plan.Descendants, want) {
		t.Fatalf("descendants = %v, want each known descendant exactly once: %v", plan.Descendants, want)
	}
}
