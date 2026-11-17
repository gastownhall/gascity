package exec

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSeamBackedResolvesProgramStatus(t *testing.T) {
	for _, seam := range []bool{false, true} {
		var p runtime.Provider = NewProvider("unused")
		if seam {
			p = NewSeamBacked("unused")
		}
		if _, ok := runtime.ProgramStatusFor(p, "s"); !ok {
			t.Errorf("seam=%v: provider does not expose program status", seam)
		}
	}
}

func TestProgramStatusDecidesIdleAtBase(t *testing.T) {
	for _, seam := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "seams"}[seam], func(t *testing.T) {
			pk := newOpsPack(t, `"carrier.ops","idle-boundary","report-activity","report-attachment","program-status"`)
			pk.put(t, "program-status.body", `echo '{"state":"idle"}'`)
			pk.put(t, "peek.body", `printf '%s\n' 'Working… (esc to interrupt)' '❯ '`)
			var p runtime.Provider = NewProvider(pk.script)
			if seam {
				p = NewSeamBacked(pk.script)
			}
			setter, ok := p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			})
			if !ok {
				t.Error("provider does not take the program-status switch")
			} else {
				setter.SetProgramStatusMode(runtime.ProgramStatusOn)
			}
			idle := p.(runtime.IdleWaitProvider)
			if err := idle.WaitForIdle(context.Background(), "s", 700*time.Millisecond); err != nil {
				t.Errorf("status idle over a busy screen: %v", err)
			}
			pk.noTmux(t)
		})
	}
}
