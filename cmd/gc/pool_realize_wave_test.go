package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// poolWaveFixture is one realization over a city whose session census spans
// a primary store and one store per rig, the shape of a multi-rig city whose
// every guarded create re-reads all of them under its identifier locks.
type poolWaveFixture struct {
	cfg       *config.City
	bp        *agentBuildParams
	primary   beads.Store
	rigStores map[string]beads.Store
	stderr    *bytes.Buffer
	targets   []poolRealizeTarget
}

// newPoolWaveFixture builds cfg's build params over primary and one MemStore
// per configured rig, primed with a complete census. Each agent gets one
// pool request per entry in requests (default one).
func newPoolWaveFixture(t *testing.T, cfg *config.City, primary beads.Store, requests map[string]int) *poolWaveFixture {
	t.Helper()
	cityPath := t.TempDir()
	rigStores := make(map[string]beads.Store, len(cfg.Rigs))
	for i := range cfg.Rigs {
		cfg.Rigs[i].Path = filepath.Join(cityPath, "rigs", cfg.Rigs[i].Name)
		rigStores[cfg.Rigs[i].Name] = beads.NewMemStore()
	}
	stderr := &bytes.Buffer{}
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), primary, stderr)
	primeGuardedPoolCrossStoreCensus(t, bp, rigStores)
	f := &poolWaveFixture{cfg: cfg, bp: bp, primary: primary, rigStores: rigStores, stderr: stderr}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		template := agent.QualifiedName()
		n, ok := requests[template]
		if !ok {
			n = 1
		}
		state := PoolDesiredState{Template: template}
		for j := 0; j < n; j++ {
			state.Requests = append(state.Requests, SessionRequest{Template: template, Tier: "new"})
		}
		f.targets = append(f.targets, poolRealizeTarget{cfgAgent: agent, state: state})
	}
	return f
}

// realizeSerially realizes the fixture's pools one at a time, each pool's
// creates finishing before the next pool plans: the reference order the
// common create wave must reproduce.
func (f *poolWaveFixture) realizeSerially() map[string]TemplateParams {
	desired := map[string]TemplateParams{}
	for _, target := range f.targets {
		realizePoolDesiredSessionsAt(f.bp, target.cfgAgent, target.state, time.Now(), desired, f.stderr)
	}
	return desired
}

// realizeInWave realizes every pool of the fixture in one call.
func (f *poolWaveFixture) realizeInWave() (map[string]TemplateParams, poolRealizeStats) {
	desired := map[string]TemplateParams{}
	stats := realizePoolsDesiredSessionsAt(f.bp, f.targets, time.Now(), desired, f.stderr)
	return desired, stats
}

// poolWaveOutcome renders a realization's durable result without bead IDs:
// every open session row in the primary store and every desired entry, by
// identity. Two realizations agree when their outcomes are equal.
func poolWaveOutcome(t *testing.T, f *poolWaveFixture, desired map[string]TemplateParams) string {
	t.Helper()
	infos, err := sessionFrontDoor(f.primary).ListAll(sessionpkg.ListAllOptions{})
	if err != nil {
		t.Fatalf("listing created sessions: %v", err)
	}
	var lines []string
	for _, info := range infos {
		if info.Closed {
			continue
		}
		lines = append(lines, fmt.Sprintf("row template=%s agent=%s alias=%s slot=%s", info.Template, info.AgentName, info.Alias, info.PoolSlot))
	}
	for _, tp := range desired {
		lines = append(lines, fmt.Sprintf("desired template=%s instance=%s alias=%s slot=%d", tp.TemplateName, tp.InstanceName, tp.Alias, tp.PoolSlot))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func tenRigPoolCity(maxWakes int) *config.City {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
	}
	for i := 0; i < 10; i++ {
		rig := fmt.Sprintf("rig%02d", i)
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: rig})
		cfg.Agents = append(cfg.Agents, config.Agent{
			Name:              "worker",
			Dir:               rig,
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		})
	}
	return cfg
}

// TestRealizePools_TenRigOneCreatePoolsShareOneCreateWave pins the latency
// fix: ten pools with one fresh create each run their creates as one bounded
// wave rather than one pool after another. Serially, at most one create is
// ever in flight; the wave reaches the realization's worker cap.
func TestRealizePools_TenRigOneCreatePoolsShareOneCreateWave(t *testing.T) {
	const createDelay = 50 * time.Millisecond

	serialStore := &delayingPoolCreateStore{MemStore: beads.NewMemStore(), delay: createDelay}
	serial := newPoolWaveFixture(t, tenRigPoolCity(10), serialStore, nil)
	serialDesired := serial.realizeSerially()
	if got := serialStore.maxConcurrentSessionCreates(); got != 1 {
		t.Fatalf("serial reference max concurrent creates = %d, want 1 (one-create pools cannot overlap)", got)
	}

	waveStore := &delayingPoolCreateStore{MemStore: beads.NewMemStore(), delay: createDelay}
	wave := newPoolWaveFixture(t, tenRigPoolCity(10), waveStore, nil)
	waveDesired, stats := wave.realizeInWave()

	if got := waveStore.maxConcurrentSessionCreates(); got < poolRealizeParallelism {
		t.Fatalf("wave max concurrent creates = %d, want at least %d across pools; stderr=%q", got, poolRealizeParallelism, wave.stderr.String())
	}
	if stats.creates != 10 || stats.createErrors != 0 || stats.realized != 10 || stats.waves != 1 {
		t.Fatalf("wave stats = %+v, want 10 creates, 0 errors, 10 realized in 1 wave", stats)
	}
	if got, want := poolWaveOutcome(t, wave, waveDesired), poolWaveOutcome(t, serial, serialDesired); got != want {
		t.Fatalf("wave outcome differs from serial reference:\n got:\n%s\nwant:\n%s", got, want)
	}
	for _, tp := range waveDesired {
		if tp.PoolSlot != 1 {
			t.Fatalf("desired %s slot = %d, want deterministic slot 1 for each pool's only create", tp.InstanceName, tp.PoolSlot)
		}
	}
}

// TestRealizePools_WaveMatchesSerialOnCrossPoolCollisions pins that a pool
// whose planning could read a row an earlier pool of the wave has planned
// but not yet written plans only after that row lands, and that creates
// contending for one identifier run in pool order. Each case runs many times
// because a wave that raced those creates would disagree only sometimes.
func TestRealizePools_WaveMatchesSerialOnCrossPoolCollisions(t *testing.T) {
	cases := []struct {
		name string
		city func() *config.City
		want string
	}{
		{
			// Two pools drawing names from one namepool in one rig: the second
			// pool's slot claim reads the first pool's fresh "rig/alice" row as
			// its own slot 1, so it takes "rig/bob".
			name: "shared namepool",
			city: func() *config.City {
				names := []string{"alice", "bob"}
				return &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Rigs:      []config.Rig{{Name: "rig"}},
					Agents: []config.Agent{
						{Name: "alpha", Dir: "rig", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2), Namepool: "names.txt", NamepoolNames: names},
						{Name: "beta", Dir: "rig", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2), Namepool: "names.txt", NamepoolNames: names},
					},
				}
			},
		},
		{
			// A numbered pool's slot identity "rig/w-1" is a singleton
			// agent's canonical identity: the singleton reads it as held.
			name: "canonical singleton held by another pool's slot",
			city: func() *config.City {
				return &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Rigs:      []config.Rig{{Name: "rig"}},
					Agents: []config.Agent{
						{Name: "w", Dir: "rig", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
						{Name: "w-1", Dir: "rig", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
					},
				}
			},
		},
		{
			// Two pools in different rigs resolving one literal tmux_alias:
			// both creates lock "shared-box", and the earlier pool wins it.
			name: "shared tmux_alias across rigs",
			city: func() *config.City {
				return &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Rigs:      []config.Rig{{Name: "east"}, {Name: "west"}},
					Agents: []config.Agent{
						{Name: "box", Dir: "east", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2), TmuxAlias: "shared-box"},
						{Name: "box", Dir: "west", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2), TmuxAlias: "shared-box"},
					},
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serial := newPoolWaveFixture(t, tc.city(), beads.NewMemStore(), nil)
			serialDesired := serial.realizeSerially()
			want := poolWaveOutcome(t, serial, serialDesired)
			if want == "" {
				t.Fatal("serial reference realized nothing; the fixture is not exercising a collision")
			}
			for run := 0; run < 20; run++ {
				wave := newPoolWaveFixture(t, tc.city(), &delayingPoolCreateStore{MemStore: beads.NewMemStore(), delay: time.Millisecond}, nil)
				waveDesired, _ := wave.realizeInWave()
				if got := poolWaveOutcome(t, wave, waveDesired); got != want {
					t.Fatalf("run %d: wave outcome differs from serial reference:\n got:\n%s\nwant:\n%s\nstderr=%q", run, got, want, wave.stderr.String())
				}
			}
		})
	}
}

// templateFailingPoolCreateStore fails every session-bead create of one
// template.
type templateFailingPoolCreateStore struct {
	*beads.MemStore
	template string
}

func (s *templateFailingPoolCreateStore) Create(bead beads.Bead) (beads.Bead, error) {
	if bead.Type == sessionBeadType && bead.Metadata["template"] == s.template {
		return beads.Bead{}, errors.New("injected pool create failure")
	}
	return s.MemStore.Create(bead)
}

// TestRealizePools_FailedCreateRefundReachesLaterPoolInWave pins the budget
// contract the serial loop gave: a create that fails returns its token, and a
// later pool whose fresh create the exhausted budget deferred claims it in
// the same realization. Here beta's create fails; gamma's first request takes
// the refund and its second stays deferred, as it would serially.
func TestRealizePools_FailedCreateRefundReachesLaterPoolInWave(t *testing.T) {
	city := func() *config.City {
		maxWakes := 2
		return &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
			Agents: []config.Agent{
				{Name: "alpha", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
				{Name: "beta", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
				{Name: "gamma", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			},
		}
	}
	requests := map[string]int{"gamma": 2}
	serial := newPoolWaveFixture(t, city(), &templateFailingPoolCreateStore{MemStore: beads.NewMemStore(), template: "beta"}, requests)
	serialDesired := serial.realizeSerially()
	wave := newPoolWaveFixture(t, city(), &templateFailingPoolCreateStore{MemStore: beads.NewMemStore(), template: "beta"}, requests)
	waveDesired, stats := wave.realizeInWave()

	if got, want := poolWaveOutcome(t, wave, waveDesired), poolWaveOutcome(t, serial, serialDesired); got != want {
		t.Fatalf("wave outcome differs from serial reference:\n got:\n%s\nwant:\n%s\nstderr=%q", got, want, wave.stderr.String())
	}
	templates := map[string]int{}
	for _, tp := range waveDesired {
		templates[tp.TemplateName]++
	}
	if templates["alpha"] != 1 || templates["beta"] != 0 || templates["gamma"] != 1 {
		t.Fatalf("realized templates = %v, want alpha=1 beta=0 gamma=1 (gamma takes beta's refund)", templates)
	}
	if stats.creates != 3 || stats.createErrors != 1 || stats.waves != 2 {
		t.Fatalf("stats = %+v, want 3 creates (1 failed) over 2 waves", stats)
	}
	if !strings.Contains(wave.stderr.String(), "injected pool create failure") {
		t.Fatalf("stderr = %q, want beta's create failure reported", wave.stderr.String())
	}
	if got := strings.Count(wave.stderr.String(), "fresh create deferred"); got != 1 {
		t.Fatalf("stderr reports %d deferred creates, want exactly gamma's second request; stderr=%q", got, wave.stderr.String())
	}
}

// TestRealizePools_PartialLockedCensusFailsEveryWaveCreateClosed pins the
// lock-time proof inside the wave: a rig leg that degrades after planning
// leaves every create's availability unprovable, so none writes and each
// pool's reserved slot is released.
func TestRealizePools_PartialLockedCensusFailsEveryWaveCreateClosed(t *testing.T) {
	primary := beads.NewMemStore()
	f := newPoolWaveFixture(t, tenRigPoolCity(10), primary, nil)
	failing := &toggleListFailStore{Store: f.rigStores["rig03"]}
	f.rigStores["rig03"] = failing
	f.bp.sessionCensusRigStores = cloneSessionCensusRigStores(f.rigStores)
	failing.fail = true

	desired, stats := f.realizeInWave()

	if len(desired) != 0 || stats.creates != 10 || stats.createErrors != 10 {
		t.Fatalf("desired=%d stats=%+v, want every one of 10 creates refused", len(desired), stats)
	}
	infos, err := sessionFrontDoor(primary).ListAll(sessionpkg.ListAllOptions{})
	if err != nil || len(infos) != 0 {
		t.Fatalf("primary sessions = %d err=%v, want none written", len(infos), err)
	}
	if got := strings.Count(f.stderr.String(), "checking locked pool availability"); got != 10 {
		t.Fatalf("stderr reports %d locked-availability refusals, want 10; stderr=%q", got, f.stderr.String())
	}

	// The released slots are free again: once the leg recovers, the next
	// realization creates slot 1 in every pool.
	failing.fail = false
	desired, stats = f.realizeInWave()
	if len(desired) != 10 || stats.createErrors != 0 {
		t.Fatalf("after recovery desired=%d stats=%+v, want 10 created", len(desired), stats)
	}
	for _, tp := range desired {
		if tp.PoolSlot != 1 {
			t.Fatalf("after recovery %s slot = %d, want released slot 1", tp.InstanceName, tp.PoolSlot)
		}
	}
}

// TestPoolCreateLanesGroupContendingCreates pins the lane partition: creates
// whose lock sets intersect, directly or through another create, share one
// lane in wave order; disjoint creates get their own lanes.
func TestPoolCreateLanesGroupContendingCreates(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "east"}, {Name: "west"}},
		Agents: []config.Agent{
			{Name: "box", Dir: "east", StartCommand: "true", MaxActiveSessions: intPtr(2), TmuxAlias: "shared-box"},
			{Name: "worker", Dir: "east", StartCommand: "true", MaxActiveSessions: intPtr(2)},
			{Name: "box", Dir: "west", StartCommand: "true", MaxActiveSessions: intPtr(2), TmuxAlias: "shared-box"},
		},
	}
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), beads.NewMemStore(), &bytes.Buffer{})
	plan := func(agent *config.Agent, slot int) *poolSessionCreatePlan {
		_, qualified, poolSlot := poolDesiredRequestIdentity(agent, slot)
		return &poolSessionCreatePlan{qualifiedInstance: qualified, slot: slot, poolSlot: poolSlot}
	}
	pools := make([]*poolRealization, len(cfg.Agents))
	for i := range cfg.Agents {
		pools[i] = newPoolRealization(i, &cfg.Agents[i], 1)
		pools[i].items = []poolRealizeWorkItem{{plan: plan(&cfg.Agents[i], 1)}}
	}
	lanes := poolCreateLanes(bp, pools)
	var got []string
	for _, lane := range lanes {
		var members []string
		for _, ref := range lane {
			members = append(members, ref.pr.qualifiedName)
		}
		got = append(got, strings.Join(members, "+"))
	}
	want := []string{"east/box+west/box", "east/worker"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lanes = %v, want %v", got, want)
	}
}
