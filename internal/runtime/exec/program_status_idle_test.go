package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProgramStatusIdleBudgetSequences(t *testing.T) {
	for _, ready := range []bool{false, true} {
		for _, tc := range []struct {
			name         string
			status, pane int
			hang         int
			source       []idleSource
		}{
			{"A", 0, 2, 0, []idleSource{sourcePane, sourcePane}},
			{"B", 2, 0, 0, []idleSource{sourceStatus, sourceStatus}},
			{"C", 2, 2, 0, []idleSource{sourceStatus, sourcePane, sourcePane}},
			{"Cprime", 1, 2, 1, []idleSource{sourceStatus, sourcePane, sourcePane}},
			{"D", 3, 2, 0, []idleSource{sourceStatus, sourcePane, sourcePane}},
			{"E", 2, 2, 0, []idleSource{sourcePane, sourcePane}},
			{"F", 0, 2, 1, []idleSource{sourcePane, sourcePane}},
		} {
			for _, atCeiling := range []bool{false, true} {
				t.Run(fmt.Sprint(tc.name, ready, atCeiling), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						trips := tc.status + tc.pane
						if ready {
							trips++
						}
						pauses := len(tc.source) - 1
						available := 5*time.Second - time.Duration(pauses)*execIdlePollInterval - time.Duration(tc.hang)*time.Second
						x := available / time.Duration(trips)
						if !atCeiling {
							x -= time.Millisecond
						} else {
							x += time.Nanosecond
						}
						before := time.Now()
						if ready {
							<-time.After(x)
						}
						index := 0
						r := observationFunc(func(ctx context.Context, _, _ string) (idleObservation, error) {
							i := min(index, len(tc.source)-1)
							index++
							cost := x
							switch tc.name {
							case "A", "B":
							case "C":
								if i == 1 {
									cost = 2 * x
								}
							case "Cprime":
								if i == 1 {
									cost = time.Second + x
								}
							case "D":
								if i > 0 {
									cost = 2 * x
								}
							case "E":
								cost = 2 * x
							case "F":
								if i == 0 {
									cost = time.Second + x
								}
							}
							select {
							case <-time.After(cost):
								return idleObservation{readingIdle, tc.source[i]}, nil
							case <-ctx.Done():
								return idleObservation{}, ctx.Err()
							}
						})
						err := waitForIdleReadings(t.Context(), r, time.Now, "s", "❯ ", 5*time.Second-time.Since(before))
						if (err != nil) != atCeiling {
							t.Errorf("x=%s error=%v, at ceiling=%v", x, err, atCeiling)
						}
						if time.Since(before) > 5*time.Second {
							t.Errorf("hard bound exceeded: %s", time.Since(before))
						}
					})
				})
			}
		}
	}
}

// The algebraic ceilings above are independent of the status op's 1s cap.
// This proof runs every sequence through the real reader with bounded in-memory
// ops; successful status latencies stay below that cap.
func TestProgramStatusReaderBudgetSequences(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, ready := range []bool{false, true} {
			for _, sequence := range []string{"A", "B", "C", "Cprime", "D", "E", "F"} {
				t.Run(fmt.Sprint(name, ready, sequence), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						p := ctor("unused")
						var raw *Provider
						switch v := p.(type) {
						case *Provider:
							raw = v
						case *seamBackedProvider:
							raw = v.raw
						}
						mode := runtime.ProgramStatusOn
						if sequence == "A" {
							mode = runtime.ProgramStatusOff
						}
						p.(interface {
							SetProgramStatusMode(runtime.ProgramStatusMode)
						}).SetProgramStatusMode(mode)
						const latency = 200 * time.Millisecond
						start := time.Now()
						prefix := "❯ "
						if ready {
							prefix = raw.readyPromptPrefix(t.Context(), "s", 5*time.Second, func(ctx context.Context) (string, bool, error) {
								select {
								case <-time.After(latency):
									return "❯ ", true, nil
								case <-ctx.Done():
									return "", false, ctx.Err()
								}
							})
						}
						calls := 0
						r := &statusPreferringReader{p: raw, pane: paneIdleReader{delayedStatusPane{latency: latency}}, read: func(ctx context.Context, _ string, bound time.Duration) (runtime.ProgramStatus, error) {
							calls++
							call := calls
							status, expired, err := bounded(ctx, bound, func(ctx context.Context) (runtime.ProgramStatus, error) {
								if sequence == "F" || sequence == "Cprime" && call == 2 {
									<-ctx.Done()
									return runtime.ProgramStatus{}, ctx.Err()
								}
								select {
								case <-time.After(latency):
								case <-ctx.Done():
									return runtime.ProgramStatus{}, ctx.Err()
								}
								if sequence == "C" && call == 2 {
									return runtime.ProgramStatus{}, runtime.ErrRuntimeUnavailable
								}
								if sequence == "E" || sequence == "D" && call > 1 {
									return runtime.ProgramStatus{Mode: mode, Validity: runtime.ProgramStatusSilent}, nil
								}
								return runtime.ProgramStatus{Mode: mode, Validity: runtime.ProgramStatusValid, State: runtime.ProgramIdle}, nil
							})
							if expired {
								return runtime.ProgramStatus{}, runtime.ErrRuntimeUnavailable
							}
							return status, err
						}}
						if err := waitForIdleReadings(t.Context(), r, time.Now, "s", prefix, 5*time.Second-time.Since(start)); err != nil {
							t.Fatal(err)
						}
						want := map[string]int{"A": 0, "B": 2, "C": 2, "Cprime": 2, "D": 3, "E": 2, "F": 1}[sequence]
						if calls != want {
							t.Errorf("%d status reads, want %d", calls, want)
						}
						if time.Since(start) >= 5*time.Second {
							t.Error("proof exceeded budget")
						}
					})
				})
			}
		}
	}
}

type delayedStatusPane struct {
	runtime.Carrier
	latency time.Duration
}

func (c delayedStatusPane) Peek(ctx context.Context, _ string, _ int) (string, error) {
	select {
	case <-time.After(c.latency):
		return idleClaudePane, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestProgramStatusRealScriptWaitBothCarriers(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, ops := range []bool{false, true} {
			for _, fails := range []bool{false, true} {
				t.Run(fmt.Sprint(name, ops, fails), func(t *testing.T) {
					caps := `"report-activity","report-attachment","idle-boundary","program-status","proc.exec"`
					if ops {
						caps += `,"carrier.ops"`
					}
					pk := newOpsPack(t, caps)
					pk.put(t, "screen", idleClaudePane)
					pk.put(t, "exec.body", `cmd=$(cat); printf 'EXEC %s\n' "$cmd" >> "$D/log"; case "$cmd" in *show-environment*) printf 'GC_READY_PROMPT_PREFIX=❯ \n' ;; *capture-pane*) cat "$D/screen" ;; esac`)
					pk.put(t, "program-status.body", `echo '{"state":"idle"}'`)
					if fails {
						pk.put(t, "program-status.body", `exit 1`)
					} else {
						pk.put(t, "screen", busyClaudePane)
					}
					p := ctor(pk.script)
					p.(interface {
						SetProgramStatusMode(runtime.ProgramStatusMode)
					}).SetProgramStatusMode(runtime.ProgramStatusOn)
					if err := p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", 2*time.Second); err != nil {
						t.Fatal(err)
					}
					if ops {
						pk.noTmux(t)
					}
					count := strings.Count(pk.log(t), "OP program-status")
					if fails && count != 1 || !fails && count != 2 {
						t.Errorf("status ops %d", count)
					}
				})
			}
		}
	}
}

func TestProgramStatusDoesNotGrantIdleBoundary(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, `"program-status","carrier.ops","report-activity","report-attachment"`)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			if err := p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", time.Second); !errors.Is(err, runtime.ErrInteractionUnsupported) {
				t.Error(err)
			}
			if p.(runtime.SleepCapabilityProvider).SleepCapability("s") != runtime.SessionSleepCapabilityTimedOnly {
				t.Error("token granted full sleep")
			}
			if strings.Contains(pk.log(t), "OP program-status") {
				t.Error("unsupported wait read status")
			}
		})
	}
}

func TestShadowWaitSendsNoStatusOp(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, opsIdleCaps+`,"program-status"`)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusShadow)
			if err := p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", time.Second); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(pk.log(t), "OP program-status") {
				t.Error("shadow wait read status")
			}
		})
	}
}

func TestProgramStatusBlockedAndIndeterminateIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, state := range []runtime.ProgramState{runtime.ProgramBlocked, runtime.ProgramWorking, ""} {
			p := NewProvider("unused", WithProgramStatusMode(runtime.ProgramStatusOn))
			since := time.Now()
			if state == runtime.ProgramWorking {
				since = since.Add(-runtime.ProgramStatusHoldLimit)
			}
			r := &statusPreferringReader{p: p, pane: paneIdleReader{readingCarrier{screen: idleClaudePane}}, read: func(context.Context, string, time.Duration) (runtime.ProgramStatus, error) {
				return runtime.ProgramStatus{Mode: runtime.ProgramStatusOn, Validity: runtime.ProgramStatusValid, State: state, Since: since}, nil
			}}
			err := waitForIdleReadings(t.Context(), r, time.Now, "s", "❯ ", time.Second)
			if state == runtime.ProgramBlocked {
				if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, runtime.ErrWaitUnobserved) || errors.Is(err, runtime.ErrPendingInteraction) {
					t.Errorf("blocked: %v", err)
				}
			} else if err != nil {
				t.Errorf("indeterminate fallback: %v", err)
			}
		}
	})
}

func TestProgramStatusHungReadAndLateResult(t *testing.T) {
	old := programStatusIdleReadBound
	programStatusIdleReadBound = 40 * time.Millisecond
	t.Cleanup(func() { programStatusIdleReadBound = old })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, opsIdleCaps+`,"program-status"`)
			pk.put(t, "program-status.body", `trap '' TERM; sleep 0.15; echo '{"state":"working","incarnation":"late"}'`)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			start := time.Now()
			err := p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", time.Second)
			if err != nil || time.Since(start) > time.Second {
				t.Fatalf("wait %s: %v", time.Since(start), err)
			}
			var raw *Provider
			switch v := p.(type) {
			case *Provider:
				raw = v
			case *seamBackedProvider:
				raw = v.raw
			}
			// A failed newer outcome is what readers see, never the abandoned success.
			got, err := p.(runtime.ProgramStatusProvider).ProgramStatus("s")
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || got.Valid() {
				t.Errorf("late outcome: %+v %v", got, err)
			}
			raw.programs.mu.Lock()
			record := raw.programs.sessions["s"].record
			raw.programs.mu.Unlock()
			if record.State != "" {
				t.Errorf("abandoned read mutated record: %+v", record)
			}
		})
	}
}

func TestProgramStatusModeRace(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			_, p, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
			var wg sync.WaitGroup
			wg.Go(func() {
				for range 20 {
					p.(interface {
						SetProgramStatusMode(runtime.ProgramStatusMode)
					}).SetProgramStatusMode(runtime.ProgramStatusOff)
					p.(interface {
						SetProgramStatusMode(runtime.ProgramStatusMode)
					}).SetProgramStatusMode(runtime.ProgramStatusOn)
				}
			})
			wg.Go(func() {
				for range 20 {
					_, _ = ps.ProgramStatus("s")
				}
			})
			wg.Wait()
		})
	}
}
