package exec

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestUndeclaredPackOpLogUnchanged(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, ops := range []bool{false, true} {
			t.Run(fmt.Sprint(name, ops), func(t *testing.T) {
				capture := "OP exec s\nEXEC 'tmux' 'capture-pane' '-t' 'main' '-p' '-S' '-120'\n"
				publish := "OP exec s\nEXEC 'tmux' '-u' 'set-environment' '-t' 'main' 'GC_READY_PROMPT_PREFIX' '❯ '\n"
				if ops {
					capture = "OP peek s 120\n"
					publish = "OP set-meta s GC_READY_PROMPT_PREFIX\n"
				}
				expected := "OP protocol\nOP observe s\nOP start s\n" + publish + "OP inventory\nOP inventory\n" + capture + capture
				if name == "seams" {
					expected += "OP observe s\n"
				}
				expected += capture
				if name == "seams" {
					expected += "OP is-running s\nOP is-running s\n"
				}
				expected += "OP stop s\n"
				for _, declares := range []bool{false, true} {
					for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
						if declares && mode != runtime.ProgramStatusOff {
							continue
						}
						caps := `"observe.v1","report-attachment","report-activity","idle-boundary","proc.exec"`
						if ops {
							caps += `,"carrier.ops"`
						}
						if declares {
							caps += `,"program-status"`
						}
						pk := newOpsPack(t, caps)
						pk.put(t, "inventory.body", `echo '{"complete":true,"sessions":[{"name":"s","state":"running","object":{"id":"b","created":"t"}}]}'`)
						pk.put(t, "exec.body", `cmd=$(cat); printf 'EXEC %s\n' "$cmd" >> "$D/log"; case "$cmd" in *capture-pane*) cat "$D/screen" ;; *show-environment*) printf 'GC_READY_PROMPT_PREFIX=❯ \n' ;; esac`)
						p := ctor(pk.script)
						if setter, ok := p.(interface {
							SetProgramStatusMode(runtime.ProgramStatusMode)
						}); ok {
							setter.SetProgramStatusMode(mode)
						}
						if err := p.Start(t.Context(), "s", runtime.Config{AcceptStartupDialogs: new(bool)}); err != nil {
							t.Fatal(err)
						}
						if ps, ok := runtime.ProgramStatusFor(p, "s"); ok {
							if _, err := ps.ProgramStatus("s"); err != nil {
								t.Fatal(err)
							}
						}
						if _, err := p.ListRunning(""); err != nil {
							t.Fatal(err)
						}
						if _, _, _, err := runtime.ListInventoryWithAttestation(p, ""); err != nil {
							t.Fatal(err)
						}
						if err := p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", time.Second); err != nil {
							t.Fatal(err)
						}
						if _, err := p.Peek("s", 120); err != nil {
							t.Fatal(err)
						}
						if err := p.Stop("s"); err != nil {
							t.Fatal(err)
						}
						log := pk.log(t)
						if strings.Contains(log, "OP program-status") {
							t.Fatal("undeclared op sent")
						}
						if log != expected {
							t.Errorf("mode %s changed the fixed base op/exec-stdin log:\n%s\nwant:\n%s", mode, log, expected)
						}
						if ops {
							pk.noTmux(t)
						}
					}
				}
			})
		}
	}
}
