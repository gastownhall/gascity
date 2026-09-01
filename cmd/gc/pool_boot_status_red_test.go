package main

import (
	"io"
	"reflect"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
)

func TestPhasesCompletedBeforeRecognizesPoolOnBoot(t *testing.T) {
	want := []string{
		"loading_config",
		"starting_bead_store",
		"resolving_formulas",
		"adopting_sessions",
		"starting_agents",
	}
	if got := phasesCompletedBefore("running_pool_on_boot"); !reflect.DeepEqual(got, want) {
		t.Fatalf("phasesCompletedBefore(running_pool_on_boot) = %v, want %v", got, want)
	}
}

func TestStatusDisplayTextFormatsStructuredPoolBootProgress(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		progress *poolBootDisplayProgress
		want     string
	}{
		{
			name:   "ordinary phase without progress",
			status: "starting_agents",
			want:   "Starting agents...",
		},
		{
			name:   "pool phase before progress is known",
			status: "running_pool_on_boot",
			want:   "running_pool_on_boot...",
		},
		{
			name:     "empty optional progress",
			status:   "running_pool_on_boot",
			progress: &poolBootDisplayProgress{},
			want:     "running_pool_on_boot...",
		},
		{
			name:   "counts without current agent",
			status: "running_pool_on_boot",
			progress: &poolBootDisplayProgress{
				Done:  2,
				Total: 5,
			},
			want: "running_pool_on_boot 2/5...",
		},
		{
			name:   "counts and arbitrary agent identity",
			status: "running_pool_on_boot",
			progress: &poolBootDisplayProgress{
				Done:  3,
				Total: 5,
				Agent: "rig:blue/worker:slot/one",
			},
			want: "running_pool_on_boot 3/5 (rig:blue/worker:slot/one)...",
		},
		{
			name:   "stale progress is ignored outside pool phase",
			status: "starting_agents",
			progress: &poolBootDisplayProgress{
				Done:  3,
				Total: 5,
				Agent: "rig:blue/worker:slot/one",
			},
			want: "Starting agents...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			city := api.CityInfo{Status: tt.status}
			if tt.progress != nil {
				setDisplayPoolBoot(t, &city, *tt.progress)
			}
			if got := statusDisplayTextWithPoolBoot(city); got != tt.want {
				t.Fatalf("statusDisplayTextWithPoolBoot(%q, %#v) = %q, want %q", tt.status, tt.progress, got, tt.want)
			}
		})
	}
}

func TestRunPoolOnBootReportsSerializedCompletionProgress(t *testing.T) {
	zero := 0
	maxSessions := 2
	configuredAgents := []string{
		"rig:blue/worker:slot/one",
		"rig/green/reviewer:slot/two",
	}
	cfg := &config.City{Agents: []config.Agent{
		{Name: configuredAgents[0], MinActiveSessions: &zero, MaxActiveSessions: &maxSessions, OnBoot: "hook-one"},
		{Name: configuredAgents[1], MinActiveSessions: &zero, MaxActiveSessions: &maxSessions, OnBoot: "hook-two"},
	}}

	var mu sync.Mutex
	completedCommands := make(map[string]bool)
	runner := func(command, _ string, _ map[string]string) (string, error) {
		mu.Lock()
		completedCommands[command] = true
		mu.Unlock()
		return "", nil
	}

	type progressEvent struct {
		done  int
		total int
		agent string
	}
	var events []progressEvent
	runPoolOnBootWithProgress(cfg, t.TempDir(), runner, io.Discard, func(done, total int, agent string) {
		mu.Lock()
		defer mu.Unlock()
		command := map[string]string{
			configuredAgents[0]: "hook-one",
			configuredAgents[1]: "hook-two",
		}[agent]
		if command == "" {
			t.Errorf("progress agent = %q, want an exact configured identity", agent)
		} else if !completedCommands[command] {
			t.Errorf("progress for %q arrived before its hook completed", agent)
		}
		events = append(events, progressEvent{done: done, total: total, agent: agent})
	})

	mu.Lock()
	defer mu.Unlock()
	if len(completedCommands) != len(configuredAgents) {
		t.Fatalf("completed hooks = %v, want both configured hooks", completedCommands)
	}
	if len(events) != len(configuredAgents) {
		t.Fatalf("progress events = %v, want one completion event per hook", events)
	}
	seenAgents := make(map[string]bool, len(events))
	for i, event := range events {
		if event.done != i+1 {
			t.Errorf("progress event %d done = %d, want serialized count %d", i, event.done, i+1)
		}
		if event.total != len(configuredAgents) {
			t.Errorf("progress event %d total = %d, want %d", i, event.total, len(configuredAgents))
		}
		seenAgents[event.agent] = true
	}
	for _, agent := range configuredAgents {
		if !seenAgents[agent] {
			t.Errorf("progress omitted configured agent %q; events = %v", agent, events)
		}
	}
}

type poolBootDisplayProgress struct {
	Done  int
	Total int
	Agent string
}

func setDisplayPoolBoot(t *testing.T, city *api.CityInfo, progress poolBootDisplayProgress) {
	t.Helper()
	field := reflect.ValueOf(city).Elem().FieldByName("PoolBoot")
	if !field.IsValid() {
		t.Fatal("api.CityInfo has no PoolBoot field; CLI startup output cannot consume structured progress")
	}
	if field.Kind() != reflect.Pointer || !field.CanSet() || field.Type().Elem().Kind() != reflect.Struct {
		t.Fatalf("api.CityInfo.PoolBoot kind/settable = %s/%t, want settable pointer to struct", field.Kind(), field.CanSet())
	}

	status := reflect.New(field.Type().Elem())
	setDisplayPoolBootField(t, status.Elem(), "Done", progress.Done)
	setDisplayPoolBootField(t, status.Elem(), "Total", progress.Total)
	agent := status.Elem().FieldByName("Agent")
	if !agent.IsValid() || !agent.CanSet() || agent.Kind() != reflect.String {
		t.Fatal("pool boot Agent field is missing or not a settable string")
	}
	agent.SetString(progress.Agent)
	field.Set(status)
}

func setDisplayPoolBootField(t *testing.T, status reflect.Value, name string, value int) {
	t.Helper()
	field := status.FieldByName(name)
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.Int {
		t.Fatalf("pool boot %s field is missing or not a settable int", name)
	}
	field.SetInt(int64(value))
}
