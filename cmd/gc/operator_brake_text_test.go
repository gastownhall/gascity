package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

// suspensionHandEditForm is how an operator suspends a city from any shell
// with no gc process running.
const suspensionHandEditForm = `{"city":{"suspended":true}}`

// AC-8: gc stop --help names gc suspend and shows the hand-edit form of the
// suspension file.
func TestStopHelpNamesSuspendAndTheHandEditForm(t *testing.T) {
	long := newStopCmd(io.Discard, io.Discard).Long
	for _, want := range []string{"gc suspend", suspensionHandEditForm, ".gc/runtime/suspension-state.json"} {
		if !strings.Contains(long, want) {
			t.Fatalf("gc stop --help does not show %q:\n%s", want, long)
		}
	}
}

// AC-8: the supervisor's forced-shutdown message points at gc suspend.
func TestForcedShutdownMessageNamesSuspend(t *testing.T) {
	msg := forcedShutdownMessage("bright-lights", 20*time.Second)
	for _, want := range []string{"bright-lights", "20s", "forcing shutdown", "gc suspend"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("forced-shutdown message %q does not name %q", msg, want)
		}
	}
}

// AC-8: the generated systemd unit names gc suspend and shows the hand-edit
// form of the suspension file.
func TestSystemdUnitNamesSuspendAndTheHandEditForm(t *testing.T) {
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, &supervisorServiceData{
		GCPath:  "/usr/local/bin/gc",
		LogPath: "/home/user/.gc/supervisor.log",
		GCHome:  "/home/user/.gc",
		Path:    "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gc suspend", suspensionHandEditForm, ".gc/runtime/suspension-state.json"} {
		if !strings.Contains(content, want) {
			t.Fatalf("systemd unit does not show %q:\n%s", want, content)
		}
	}
}
