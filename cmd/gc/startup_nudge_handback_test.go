package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// handbackNudge is the startup nudge the tests' leaves hand back.
const handbackNudge = "start working"

// wantQueuedHandbacks fails unless n copies of handbackNudge are queued for
// info's session in cityPath at now, each a user's submit delivered verbatim.
func wantQueuedHandbacks(t *testing.T, cityPath string, cfg *config.City, info session.Info, now time.Time, n int) {
	t.Helper()
	if cfg == nil {
		cfg = &config.City{}
	}
	target := resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
	pending, _, _, err := listQueuedNudgesForTarget(cityPath, target, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != n {
		t.Fatalf("pending %+v; want %d startup handback(s)", pending, n)
	}
	for _, got := range pending {
		if got.Message != handbackNudge || got.Source != nudgequeue.SourceSubmit || got.SessionID != info.ID {
			t.Fatalf("queued %+v; want the startup nudge for %s as a user's submit", got, info.ID)
		}
	}
}

// Kills a start whose startup nudge its program held back behind a dialog
// (design §10.2 row A20, program_status = "on") and that drops the nudge,
// queues one the leaf never handed back, or queues one from a start that
// failed: the handed-back nudge of a start that succeeded is queued for the
// session once, as a user's submit; a failed start's is taken and dropped.
func TestStartupNudgeHandbackIsQueuedForTheSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	info := session.Info{ID: "gc-41", SessionNameMetadata: "worker-session", Template: "worker", AgentName: "worker"}
	fake := runtime.NewFake()
	sp := runtime.FullFake{Fake: fake}
	now := time.Now()

	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", true, now); err != nil {
		t.Fatalf("no handback: %v", err)
	}
	wantQueuedHandbacks(t, dir, nil, info, now, 0)

	fake.SetStartupNudgeHandback("worker-session", handbackNudge)
	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", false, now); err != nil {
		t.Fatalf("failed start: %v", err)
	}
	wantQueuedHandbacks(t, dir, nil, info, now, 0)
	if _, ok := sp.TakeStartupNudgeHandback("worker-session"); ok {
		t.Fatal("a failed start's handback was left to take")
	}

	fake.SetStartupNudgeHandback("worker-session", handbackNudge)
	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", true, now); err != nil {
		t.Fatalf("handback: %v", err)
	}
	wantQueuedHandbacks(t, dir, nil, info, now, 1)
	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", true, now); err != nil {
		t.Fatalf("second take: %v", err)
	}
	wantQueuedHandbacks(t, dir, nil, info, now, 1)
}

// Kills a v2 launch-drift relaunch that drops the startup nudge its leaf
// handed back, or queues one from a relaunch that failed.
func TestV2RelaunchQueuesTheHandedBackStartupNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	tp := TemplateParams{TemplateName: "worker", Command: "run", FPExtra: map[string]string{"pool.max": "2"}}
	probe, _ := baselineCase(t, livenessAlive, desireKeep, nil, "state", "active")
	cfg := sessionCoreConfigForHashInfo(tp, probe.w.Census.Rows[probe.k].Info)
	launchOnly := []string{"session_key", "k-1", "started_config_hash", runtime.FingerprintVersion + ":stale", "started_provision_hash", runtime.ProvisionFingerprint(cfg), "started_launch_hash", runtime.FingerprintVersion + ":old-launch"}
	for _, tc := range []struct {
		name   string
		refuse bool
		queued int
	}{{"relaunched", false, 1}, {"refused", true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := baselineCase(t, livenessAlive, desireKeep, nil, append([]string{"state", "active"}, launchOnly...)...)
			row := c.w.Census.Rows[c.k].Info
			fake := runtime.NewFake()
			if err := fake.Start(t.Context(), "s-heal", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if tc.refuse {
				fake.RelaunchErrors = map[string]error{"s-heal": errors.New("box gone")}
			}
			fake.SetStartupNudgeHandback("s-heal", handbackNudge)
			c.w.CityPath = t.TempDir()
			c.w.Env = &reconcileEnv{Cfg: workerCity(1)}
			caps := txCaps{start: startCaps{sp: runtime.NewFake()}, reads: effectReads{city: c.store}}
			out, err := relaunchCall(t.Context(), caps, relaunchPlan{w: c.w, row: row, tp: tp, hash: sessionCoreConfigForHashInfo(tp, row), name: "s-heal", leaf: runtime.FullFake{Fake: fake}, now: gatherNow})
			if err != nil || out.OK == tc.refuse {
				t.Fatalf("relaunch %+v (%v); want landed %v", out, err, !tc.refuse)
			}
			wantQueuedHandbacks(t, c.w.CityPath, c.w.Env.Cfg, row, gatherNow, tc.queued)
		})
	}
}

// Kills a legacy launch-drift relaunch that drops the startup nudge its
// runtime handed back.
func TestLegacyRelaunchQueuesTheHandedBackStartupNudge(t *testing.T) {
	env, tp, sess := setupLaunchDriftResumeEnv(t)
	info := env.sessionInfo(sess.ID)
	env.sp.SetStartupNudgeHandback("worker", handbackNudge)
	b, _ := env.store.Get(sess.ID)
	stored := b.Metadata["started_config_hash"]
	current := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(tp, info))
	relaunched, _ := relaunchAgentForLaunchDrift(
		context.Background(), runtime.FullFake{Fake: env.sp}, sessionFrontDoor(env.store), info, "worker",
		tp, env.city, env.cfg, env.store, dispatchOptionSources{}, stored, current, b.Metadata["started_provision_hash"], b.Metadata["started_launch_hash"],
		[]string{"Command"}, env.rec, nil, &env.stdout, &env.stderr,
	)
	if !relaunched {
		t.Fatalf("not relaunched; stderr=%s", env.stderr.String())
	}
	wantQueuedHandbacks(t, env.city, env.cfg, env.sessionInfo(sess.ID), time.Now(), 1)
}
