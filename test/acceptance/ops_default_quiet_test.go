//go:build acceptance_a

// The [ops] gates' builtin default on the default city topologies.
//
// The three ops gates default to auto, which records only over a SQLite
// sessions store, so neither a fresh `gc init` city (its sessions store served
// through bd, a Dolt-served store) nor a file-store city records ops. Under
// the builtin default such a store is treated as off: the op-recorded verbs
// an agent runs print nothing about the op journal, gc doctor's ops-recording
// check is OK, and no ops.recording_degraded event is raised.
package acceptance_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestOpsDefaultAutoIsQuietOnAProxiedDefaultCity(t *testing.T) {
	p := newProxiedDefaultCity(t)
	p.initDefault(t)
	assertOpsDefaultQuiet(t, p.city, p.env)
}

func TestOpsDefaultAutoIsQuietOnAFileCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")
	assertOpsDefaultQuiet(t, c, testEnv)
}

// assertOpsDefaultQuiet runs the op-recorded verbs against the city's
// mayor under the builtin gate default and checks that none says anything
// about the op journal, then that doctor and the event log stay quiet too.
func assertOpsDefaultQuiet(t *testing.T, c *helpers.City, env *helpers.Env) {
	t.Helper()
	if !c.WaitForCondition(func() bool {
		out, err := c.GC("session", "list")
		return err == nil && strings.Contains(out, "mayor")
	}, 60*time.Second) {
		out, _ := c.GC("session", "list")
		t.Fatalf("no mayor session:\n%s", out)
	}
	for _, args := range [][]string{
		{"session", "nudge", "mayor", "ops default probe", "--delivery", "queue"},
		{"mail", "send", "mayor", "ops default probe", "--notify"},
		{"session", "wake", "mayor"},
	} {
		stdout, stderr, err := helpers.RunGCStreams(env, c.Dir, args...)
		if err != nil {
			t.Fatalf("gc %s: %v\nstdout=%s\nstderr=%s", strings.Join(args, " "), err, stdout, stderr)
		}
		if strings.Contains(stderr, "op journal") {
			t.Errorf("gc %s printed an op-journal line under the builtin default:\n%s", strings.Join(args, " "), stderr)
		}
	}

	out, _ := c.GCStdout("doctor", "--json", "--check", "ops-recording")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var report doctorReport
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	found := false
	for _, r := range report.Results {
		if r.Name != "ops-recording" {
			continue
		}
		found = true
		if r.Status != "ok" || !strings.Contains(r.Message, "auto (default) records nothing") {
			t.Errorf("doctor ops-recording = %s %q, want ok naming the default", r.Status, r.Message)
		}
	}
	if !found {
		t.Fatalf("doctor reported no ops-recording result:\n%s", out)
	}

	events, err := c.GCStdout("events", "--type", "ops.recording_degraded")
	if err != nil {
		t.Fatalf("gc events: %v\n%s", err, events)
	}
	if strings.Contains(events, "ops.recording_degraded") {
		t.Errorf("ops.recording_degraded raised under the builtin default:\n%s", events)
	}
}
