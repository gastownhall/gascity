package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestV2MissingProviderReportsAndRecoversByEpisode(t *testing.T) {
	f := newObserveFixture(t)
	cfg := missingProviderCity()
	cfg.Workspace.Name = "test-city"
	missing := true
	f.env.LookPath = func(command string) (string, error) {
		if missing {
			return "", errors.New("not found")
		}
		return "/bin/" + command, nil
	}
	publish := func(gen uint64) { f.cur.Store(&reconcileEnv{Gen: gen, Cfg: cfg, SP: f.sp}) }
	count := func() int {
		n := 0
		for _, e := range f.ev.Events {
			if e.Type == events.ProviderUnresolved {
				n++
			}
		}
		return n
	}
	publish(1)
	f.runPass(0)
	record := f.p.out.record.Load()
	if record.Err != "" {
		t.Fatal(record.Err)
	}
	if len(record.ProviderUnresolved) != 1 || count() != 1 {
		t.Fatalf("missing executable: groups=%+v, events=%+v, log=%s", record.ProviderUnresolved, f.ev.Events, f.stderr)
	}
	if !strings.Contains(f.stderr.String(), missingProviderCommand) {
		t.Fatal("diagnostic omitted the selected executable")
	}
	f.runPass(time.Second)
	if count() != 1 {
		t.Fatal("unchanged failure produced another event")
	}
	missing = false
	publish(2)
	f.runPass(time.Second)
	if len(f.p.out.record.Load().ProviderUnresolved) != 0 || count() != 1 {
		t.Fatal("recovery did not clear the failure")
	}
	missing = true
	publish(3)
	f.runPass(time.Second)
	if count() != 2 {
		t.Fatal("a second failure episode did not alert")
	}
	for _, e := range f.ev.Events {
		if e.Type != events.ProviderUnresolved {
			continue
		}
		decoded, registered, err := events.DecodePayload(e.Type, e.Payload)
		if err != nil || !registered {
			t.Fatalf("typed payload: %v, %v", registered, err)
		}
		payload := decoded.(events.ProviderUnresolvedPayload)
		if payload.Provider != "ghost" || payload.Command != missingProviderCommand || payload.Reconciler != "v2" || payload.DesiredSessionCount != nil {
			t.Fatalf("payload=%+v", payload)
		}
	}
}

func TestV2UnusedMissingProviderDoesNotReportDemandFailure(t *testing.T) {
	f := newObserveFixture(t)
	cfg := missingProviderCity()
	cfg.Workspace.Name = "test-city"
	cfg.NamedSessions = nil
	f.cur.Store(&reconcileEnv{Gen: 1, Cfg: cfg, SP: f.sp})
	f.env.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	f.runPass(0)
	if rec := f.p.out.record.Load(); rec.Err != "" || len(rec.ProviderUnresolved) != 0 {
		t.Fatalf("idle provider: %+v", rec)
	}
	for _, e := range f.ev.Events {
		if e.Type == events.ProviderUnresolved {
			t.Fatalf("unused provider reported as blocked demand: %+v", e)
		}
	}
}

func TestProviderUnresolvedCountsDistinctTemplatesAndSortsSample(t *testing.T) {
	set := newProviderUnresolvedSet()
	err := &config.ProviderNotInPATHError{Provider: "alias", Command: "missing"}
	for i := 20; i >= 0; i-- {
		set.record(fmt.Sprintf("worker-%02d", i), err)
	}
	set.record("worker-00", err)
	got := set.snapshot()
	if len(got) != 1 || got[0].DroppedTemplates != 21 || len(got[0].Templates) != providerUnresolvedTemplateSample || got[0].Templates[0] != "worker-00" {
		t.Fatalf("groups=%+v", got)
	}
	got[0].Templates[0] = "mutated"
	if set.snapshot()[0].Templates[0] != "worker-00" {
		t.Fatal("snapshot aliased mutable state")
	}
	if set.record("worker", errors.New("unrelated")) {
		t.Fatal("unrelated errors included")
	}
}

func TestV2MissingProviderRecordsTraceWithoutSessionRows(t *testing.T) {
	f := newObserveFixture(t)
	cfg := missingProviderCity()
	cfg.Workspace.Name = "test-city"
	f.cur.Store(&reconcileEnv{Gen: 1, Cfg: cfg, SP: f.sp})
	f.env.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	tracer := newSessionReconcilerTracer(f.env.CityPath, "test-city", f.stderr)
	rt := &plannerRuntime{planner: f.p, ready: make(chan struct{}), host: plannerHost{
		gather: f.env,
		beginTrace: func(trigger string) *sessionReconcilerTraceCycle {
			return tracer.BeginCycle(TraceTickTriggerPatrol, trigger, gatherNow, cfg)
		},
	}}
	rt.pass(gatherNow)
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(f.env.CityPath), TraceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.SiteCode == TraceSiteDesiredStateProviderUnresolved && rec.Fields["provider"] == "ghost" && rec.Fields["command"] == missingProviderCommand {
			return
		}
	}
	t.Fatalf("no provider diagnostic in %d records for a city with no session rows", len(records))
}
