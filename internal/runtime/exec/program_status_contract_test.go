package exec

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest/osc7501emitter/emit"
)

func TestExecProgramStatusContract(t *testing.T) {
	age := programStatusCacheAge
	programStatusCacheAge = 0
	t.Cleanup(func() { programStatusCacheAge = age })
	runtimetest.RunProgramStatusContract(t, runtimetest.ProgramStatusFixtures{
		Armed: func(t *testing.T, mode runtime.ProgramStatusMode, steps ...string) runtimetest.ProgramStatusSession {
			pk := newOpsPack(t, `"program-status"`)
			pk.put(t, "program-status.body", `if [ "$2" != s ] || [ ! -f "$D/running" ]; then exit 66; fi; cat "$D/answer"`)
			p := NewSeamBacked(pk.script, WithProgramStatusMode(mode))
			if err := p.Start(t.Context(), "s", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := p.Stop("s"); err != nil {
					t.Error(err)
				}
			})
			ps := p.(runtime.ProgramStatusProvider)
			store := programstatus.NewStore("inc.%1")
			var scanner programstatus.Scanner
			publish := func(t *testing.T, exited bool) {
				rec := store.Status()
				wire := struct {
					State           runtime.ProgramState     `json:"state"`
					Kind            runtime.ProgramBlockKind `json:"kind,omitempty"`
					App             string                   `json:"app,omitempty"`
					Progress        int                      `json:"progress"`
					Children        int                      `json:"children"`
					WorkingChildren int                      `json:"working_children"`
					Exited          bool                     `json:"exited,omitempty"`
					Incarnation     string                   `json:"incarnation"`
				}{rec.State, rec.Kind, rec.App, rec.Progress, rec.Children, rec.WorkingChildren, exited, rec.Nonce}
				b, err := json.Marshal(wire)
				if err != nil {
					t.Fatal(err)
				}
				pk.put(t, "answer", string(b))
				if _, err := ps.ProgramStatus("s"); err != nil {
					t.Fatal(err)
				}
			}
			publish(t, false) // Silent sessions must have an answer before any step.
			emitSteps := func(t *testing.T, steps ...string) {
				t.Helper()
				for _, step := range steps {
					prog, err := emit.ParseLines(step)
					if err != nil {
						t.Fatal(err)
					}
					var out bytes.Buffer
					outcome, err := emit.Run(t.Context(), prog, emit.Terminal{In: strings.NewReader(""), Out: &out})
					if err != nil {
						t.Fatal(err)
					}
					scanner.Feed(out.Bytes(), func(ev programstatus.Event) {
						switch ev.Kind {
						case programstatus.EventReport:
							report, err := programstatus.ParseBody(ev.Body)
							if err == nil {
								store.Apply(report, time.Now())
							}
						case programstatus.EventPromptStart:
							store.PromptStart()
						case programstatus.EventProbe:
							store.Probe()
						}
					})
					publish(t, outcome.Ended)
				}
			}
			emitSteps(t, steps...)
			return runtimetest.ProgramStatusSession{Provider: p, Name: "s", Emit: emitSteps, SetMode: func(t *testing.T, m runtime.ProgramStatusMode) {
				setter, ok := p.(interface {
					SetProgramStatusMode(runtime.ProgramStatusMode)
				})
				if !ok {
					t.Fatalf("missing mode setter on %T", p)
				}
				setter.SetProgramStatusMode(m)
			}}
		},
	})
}
