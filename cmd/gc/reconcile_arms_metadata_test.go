package main

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// identityCity has a pool of three ("worker"), a singleton pool ("solo")
// and a named session on worker ("boss").
func identityCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(3)},
			{Name: "solo", StartCommand: "true", MaxActiveSessions: intPtr(1)},
		},
		NamedSessions: []config.NamedSession{{Name: "boss", Template: "worker", Mode: "always"}},
	}
}

// Kills an identity refresh that drifts from legacy's (SESS-713, 717..719,
// POOL-046) or never converges (I20): each row's decision, applied, decides
// nothing more.
func TestDesiredRowIdentity(t *testing.T) {
	cfg := identityCity()
	for _, tc := range []struct {
		name      string
		tp        TemplateParams
		info      session.Info
		meta      session.MetadataPatch
		aliasWork bool
		alias     string
		title     bool
		labels    bool
	}{
		{
			name: "a transient pool seat's unmanaged alias clears",
			tp:   TemplateParams{TemplateName: "worker", InstanceName: "worker-2", PoolSlot: 2},
			info: session.Info{Template: "worker", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "2", AgentName: "worker-2", Alias: "alias-gc-1"},
			meta: session.MetadataPatch{}, aliasWork: true, alias: "",
		},
		{
			name: "a stale pool slot follows the allocation's (SESS-719)",
			tp:   TemplateParams{TemplateName: "worker", InstanceName: "worker-2", PoolSlot: 2},
			info: session.Info{Template: "worker", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "5", AgentName: "worker-2"},
			meta: session.MetadataPatch{"pool_slot": "2"},
		},
		{
			name: "a singleton's phantom slot collapses (POOL-046)",
			tp:   TemplateParams{TemplateName: "solo", InstanceName: "solo", Alias: "solo"},
			info: session.Info{Template: "solo", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "1", AgentName: "solo-1", Alias: "solo", Title: "solo-1", Labels: []string{"agent:solo-1"}},
			meta: session.MetadataPatch{"agent_name": "solo", "pool_slot": ""}, title: true, labels: true,
		},
		{
			name: "a named row drops its pool keys and takes its named keys",
			tp:   TemplateParams{TemplateName: "worker", InstanceName: "boss", ConfiguredNamedIdentity: "boss", ConfiguredNamedMode: "always", Alias: "boss"},
			info: session.Info{Template: "worker", SessionOrigin: "named", PoolManaged: true, PoolSlot: "1", AgentName: "boss", Alias: "boss"},
			meta: session.MetadataPatch{poolManagedMetadataKey: "", "pool_slot": "", namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "boss", namedSessionModeMetadata: "always"},
		},
		{
			name: "a pool row drops stale named keys",
			tp:   TemplateParams{TemplateName: "worker", InstanceName: "worker-2", PoolSlot: 2},
			info: session.Info{Template: "worker", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "2", AgentName: "worker-2", ConfiguredNamedSession: true, ConfiguredNamedIdentity: "old"},
			meta: session.MetadataPatch{namedSessionMetadataKey: "", namedSessionIdentityMetadata: ""},
		},
		{
			name: "a converged pool seat",
			tp:   TemplateParams{TemplateName: "worker", InstanceName: "worker-2", PoolSlot: 2},
			info: session.Info{Template: "worker", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "2", AgentName: "worker-2"},
			meta: session.MetadataPatch{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := rowRefreshKeys{"continuation_epoch": "1"}
			id := desiredRowIdentity(cfg, tc.tp, tc.info, raw)
			if !maps.Equal(id.Meta, tc.meta) || id.AliasWork != tc.aliasWork || tc.aliasWork && id.Alias != tc.alias || (id.Title != nil) != tc.title || (len(id.Add)+len(id.Remove) > 0) != tc.labels {
				t.Fatalf("identity %+v, want meta %v alias work %v (%q) title %v labels %v", id, tc.meta, tc.aliasWork, tc.alias, tc.title, tc.labels)
			}
			// I20: the write, applied, leaves nothing to decide.
			next := tc.info.ApplyPatch(identityPatch(id, tc.info, false, nil, func() session.MetadataPatch { return nil }, "T"))
			if id.Title != nil {
				next.Title = *id.Title
			}
			next.Labels = slices.DeleteFunc(append(slices.Clone(next.Labels), id.Add...), func(l string) bool { return slices.Contains(id.Remove, l) })
			again := desiredRowIdentity(cfg, tc.tp, next, raw)
			if len(identityPatch(again, next, false, nil, func() session.MetadataPatch { return nil }, "T")) != 0 || again.AliasWork || again.Title != nil || len(again.Add)+len(again.Remove) > 0 {
				t.Fatalf("not converged after its write: %+v", again)
			}
		})
	}
}

// Kills an alias write without its history, conflict clear, rebaseline or
// guarded keys, a conflict recorded every pass, and a guarded key written
// past a conflict (SESS-717/718): a free alias writes all of them; a
// conflict records its markers on the transition only, clears a managed
// pool row's wrong alias and withholds the guarded keys; a blocked alias
// writes the plain keys alone.
func TestIdentityPatch(t *testing.T) {
	id := rowIdentity{Meta: session.MetadataPatch{"session_origin": "ephemeral"}, Guarded: session.MetadataPatch{"agent_name": "worker-2"}, AliasWork: true, Alias: "worker-2", ManagedPool: true}
	info := session.Info{Alias: "wrong", StartedConfigHash: "v6:x", PoolAliasConflict: "older", PoolAliasConflictCount: "2"}
	rebaseline := func() session.MetadataPatch { return session.MetadataPatch{"started_config_hash": "v6:new"} }
	free := identityPatch(id, info, false, nil, rebaseline, "T")
	for k, v := range map[string]string{"alias": "worker-2", "alias_history": "wrong", "agent_name": "worker-2", "started_config_hash": "v6:new", poolAliasConflictMetadataKey: "", "synced_at": "T"} {
		if free[k] != v {
			t.Fatalf("free alias: patch %v, want %s=%q", free, k, v)
		}
	}
	taken := identityPatch(id, info, false, errors.New("held"), rebaseline, "T")
	if cleared, ok := taken["alias"]; taken[poolAliasConflictMetadataKey] != "worker-2" || taken[poolAliasConflictCountMetadataKey] != "3" || !ok || cleared != "" {
		t.Fatalf("conflict: patch %v, want the conflict recorded, the wrong alias cleared and no guarded key", taken)
	}
	if _, ok := taken["agent_name"]; ok {
		t.Fatalf("conflict: patch %v writes a guarded key", taken)
	}
	recorded := info
	recorded.PoolAliasConflict, recorded.Alias = "worker-2", ""
	if again := identityPatch(id, recorded, false, errors.New("held"), rebaseline, "T"); again[poolAliasConflictCountMetadataKey] != "" {
		t.Fatalf("a recorded conflict: patch %v, want no new markers", again)
	}
	if blocked := identityPatch(id, recorded, true, nil, rebaseline, "T"); !maps.Equal(blocked, session.MetadataPatch{"session_origin": "ephemeral", "synced_at": "T"}) {
		t.Fatalf("blocked: patch %v, want the plain keys alone", blocked)
	}
}

// Kills A7 winning a row it never writes (C7d-1 review must-fix 1): a
// desired canonical singleton whose metadata is converged but whose title
// and agent label are its slot's: the arm proposes the verb, whose step
// writes the title and label deltas with synced_at; applied, the arm
// proposes nothing.
func TestRowMetadataTitleOnlyWrites(t *testing.T) {
	cfg := identityCity()
	row := sessionRow("gc-1", "template", "solo", "state", "asleep", "pool_managed", "true", "session_origin", "ephemeral",
		"session_name", "s-gc-1", "agent_name", "solo", "alias", "solo", "generation", "1", "continuation_epoch", "1")
	row.Title, row.Labels = "solo-1", append(row.Labels, "agent:solo-1")
	w, a := rowWorld(t, row)
	k := rowKeyOf("gc-1")
	info := w.Census.Rows[k].Info
	w.Env = &reconcileEnv{Cfg: cfg}
	w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: TemplateParams{TemplateName: "solo", InstanceName: "solo", Alias: "solo"}}}}
	a.Snapshot.Entries[k].InDesired = true
	it, _ := decideRow(w, a, k)
	step := rowMetadataStep(txView{It: it, World: w, Alloc: a, Row: info, Now: gatherNow}, aliasCheck{}, nil)
	if it.Kind != intentRowMetadata || step.Write["synced_at"] == "" || step.Title == nil || *step.Title != "solo" || !slices.Equal(step.AddLabels, []string{"agent:solo"}) {
		t.Fatalf("decideRow %q, step %+v: want the verb writing the title and label with synced_at", it.Kind, step)
	}
	info.Title = *step.Title
	info.Labels = append(slices.DeleteFunc(slices.Clone(info.Labels), func(l string) bool { return slices.Contains(step.RemoveLabels, l) }), step.AddLabels...)
	written := newCensusRow(k, foldInfo(info, step.Write))
	written.Refresh = w.Census.Rows[k].Refresh
	w.Census.Rows[k] = written
	if it, _ := decideRow(w, a, k); it.Kind == intentRowMetadata {
		t.Fatalf("decideRow = %q after the write, want A7 converged", it.Reason)
	}
}

// Kills A7's persistent refusal winning the row (C7d-1 review should-fix
// 4): after the alias lock or an unguarded label write refused it, until
// its backoff has run out twice over, A7 writes neither the alias nor the
// title and labels, so a row with nothing else to write yields its pass;
// then it retries.
func TestRowMetadataDefersAfterItsPersistentRefusal(t *testing.T) {
	cfg := identityCity()
	row := sessionRow("gc-1", "template", "solo", "state", "asleep", "pool_managed", "true", "session_origin", "ephemeral",
		"session_name", "s-gc-1", "agent_name", "solo", "alias", "solo-old", "generation", "1", "continuation_epoch", "1")
	row.Title = "solo-1"
	w, a := rowWorld(t, row)
	k := rowKeyOf("gc-1")
	info := w.Census.Rows[k].Info
	w.Env = &reconcileEnv{Cfg: cfg}
	w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: TemplateParams{TemplateName: "solo", InstanceName: "solo", Alias: "solo"}}}}
	a.Snapshot.Entries[k].InDesired = true
	for _, cause := range []string{causeAliasLock, causeLabelsUnguarded} {
		w.Backoff = map[string]backoffRecord{rowBackoffKey(k): {Consecutive: 1, Until: gatherNow.Add(5 * time.Second), Cause: cause}}
		if it, _ := decideRow(w, a, k); it.Kind == intentRowMetadata {
			t.Fatalf("%s, inside twice its backoff: decideRow = %q, want A7 deferred", cause, it.Reason)
		}
		w.Backoff[rowBackoffKey(k)] = backoffRecord{Consecutive: 1, Until: gatherNow.Add(-vetoBackoff(1)), Cause: cause}
		if it, _ := decideRow(w, a, k); it.Kind != intentRowMetadata || it.AliasLock != "solo" {
			t.Fatalf("%s, its backoff run out twice: decideRow = (%q, lock %q), want A7 retrying under the solo lock", cause, it.Reason, it.AliasLock)
		}
	}
}

// Kills the launch and close refresh drifting from legacy's (SESS-713..715,
// SESS-701): wake_mode, command, the provider family and resume keys
// follow the resolution, an empty resolved value never clobbers a stored
// one; continuation_epoch and a session key the provider takes are
// backfilled, a stored key kept; a stale close clears; a typeless row is
// repaired; and work_dir is backfilled, alias-guarded for a legacy
// identity and never under its running session.
func TestDesiredRowLaunchRefresh(t *testing.T) {
	cfg := identityCity()
	provider := &config.ResolvedProvider{Name: "my-claude", BuiltinAncestor: "claude", ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: "resume {{.SessionKey}}", SessionIDFlag: "--session-id"}
	tp := TemplateParams{TemplateName: "worker", InstanceName: "worker-2", PoolSlot: 2, WakeMode: "fresh", Command: "run-worker", WorkDir: "/w", ResolvedProvider: provider}
	seat := session.Info{Template: "worker", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "2", AgentName: "worker-2", Type: session.BeadType, MetadataState: "asleep"}
	stale := rowRefreshKeys{"command": "old", "provider": "claude", "resume_flag": "-r", "close_reason": "orphaned", "closed_at": "T"}
	id := desiredRowIdentity(cfg, tp, seat, stale)
	want := session.MetadataPatch{
		"wake_mode": "fresh", "continuation_epoch": "1", "command": "run-worker", "provider": "my-claude", "provider_kind": "claude", "builtin_ancestor": "claude",
		"resume_flag": "--resume", "resume_style": "flag", "resume_command": "resume {{.SessionKey}}", "session_id_flag": "--session-id",
		"close_reason": "", "closed_at": "", "work_dir": "/w",
	}
	if !maps.Equal(id.Meta, want) || !id.NeedsKey || id.Type {
		t.Fatalf("identity %+v, want meta %v and a session key to mint", id, want)
	}
	converged := rowRefreshKeys{
		"wake_mode": "fresh", "continuation_epoch": "1", "command": "run-worker", "provider": "my-claude", "provider_kind": "claude", "builtin_ancestor": "claude",
		"resume_flag": "--resume", "resume_style": "flag", "resume_command": "resume {{.SessionKey}}", "session_id_flag": "--session-id", "session_key": "k-1", "work_dir": "/w",
	}
	if id := desiredRowIdentity(cfg, tp, seat, converged); len(id.Meta) != 0 || id.NeedsKey {
		t.Fatalf("converged: identity %+v, want nothing", id)
	}
	bare := tp
	bare.Command, bare.ResolvedProvider = "", nil
	if id := desiredRowIdentity(cfg, bare, seat, converged); len(id.Meta) != 0 {
		t.Fatalf("an empty resolution: meta %v, want the stored values kept", id.Meta)
	}
	typeless := seat
	typeless.Type, typeless.Labels = "", []string{session.LabelSession}
	if id := desiredRowIdentity(cfg, tp, typeless, converged); !id.Type {
		t.Fatal("a typeless session row: want its type repaired")
	}
	legacy := seat
	legacy.AgentName = "" // a legacy template identity: its work_dir waits for the alias
	for _, tc := range []struct {
		state  string
		raw    string
		guard  bool
		writes bool
	}{
		{"asleep", "", true, true},
		{"active", "", false, false},
		{"asleep", "/old", true, true},
		{"active", "/old", false, false},
		{"awake", "", false, false},
		{"", "", false, false}, // legacy's cached state reads both running (H01)
	} {
		legacy.MetadataState = tc.state
		raw := maps.Clone(converged)
		raw["work_dir"] = tc.raw
		id := desiredRowIdentity(cfg, tp, legacy, raw)
		_, guarded := id.Guarded["work_dir"]
		_, plain := id.Meta["work_dir"]
		if guarded != tc.guard || plain || (guarded || plain) != tc.writes {
			t.Fatalf("a legacy identity %s with work_dir %q: guarded %v plain %v, want guarded %v", tc.state, tc.raw, guarded, plain, tc.guard)
		}
	}
}

// Kills a session key alone left unproposed (the C7d-2 review's H11): a
// row converged but for the key its provider takes gets the verb, which
// mints it; with the key, nothing.
func TestRowMetadataProposesAMissingKeyAlone(t *testing.T) {
	cfg := identityCity()
	row := sessionRow("gc-1", "template", "worker", "state", "asleep", "pool_managed", "true", "session_origin", "ephemeral", "pool_slot", "2",
		"session_name", "s-gc-1", "agent_name", "worker-2", "generation", "1", "continuation_epoch", "1", "session_id_flag", "--session-id")
	w, a := rowWorld(t, row)
	k := rowKeyOf("gc-1")
	info := w.Census.Rows[k].Info
	tp := TemplateParams{TemplateName: "worker", InstanceName: "worker-2", PoolSlot: 2, ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}
	w.Env = &reconcileEnv{Cfg: cfg}
	w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
	a.Snapshot.Entries[k].InDesired = true
	if it, _ := decideRow(w, a, k); it.Kind != intentRowMetadata {
		t.Fatalf("decideRow = %q, want the verb for the missing key", it.Kind)
	}
	keyed := w.Census.Rows[k]
	keyed.Refresh = rowRefreshKeys{"continuation_epoch": "1", "session_id_flag": "--session-id", "session_key": "k-1"}
	w.Census.Rows[k] = keyed
	if it, _ := decideRow(w, a, k); it.Kind == intentRowMetadata {
		t.Fatalf("decideRow = %q with the key, want nothing", it.Reason)
	}
}

// Kills the census retry trigger reading other legs (the C7d-1 review's
// G05): the live check reads the sessions store alone, so a row on a rig
// leg carrying the alias does not hold it; one on the row's own leg does.
func TestAliasTakenReadsTheRowsLeg(t *testing.T) {
	cfg := identityCity()
	holder := sessionRow("gc-9", "template", "worker", "state", "asleep", "session_name", "s-gc-9", "alias", "lead")
	for _, tc := range []struct {
		name  string
		legs  []classStoreCandidate
		taken bool
	}{
		{"on the row's leg", censusLegs(rowLeg, censusStore(holder)), true},
		{"on another leg", censusLegs(rowLeg, censusStore(), "rig-a", censusStore(holder)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &World{Census: readCensus(t, gatherNow, tc.legs), Env: &reconcileEnv{Cfg: cfg}}
			if got := w.aliasTaken(rowKey{Leg: rowLeg, ID: "gc-1"}, rowIdentity{Alias: "lead"}, session.Info{ID: "gc-1"}); got != tc.taken {
				t.Fatalf("aliasTaken = %v, want %v", got, tc.taken)
			}
		})
	}
}
