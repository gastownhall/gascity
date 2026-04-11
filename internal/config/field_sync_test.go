package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestAgentFieldSync verifies that Agent, AgentPatch, and AgentOverride all
// have the same set of overridable fields. When a new field is added to Agent,
// it must also be added to AgentPatch and AgentOverride (or explicitly excluded
// below). This prevents the common bug where a new config field works in
// city.toml but is silently ignored by patches and pack overrides.
//
// See CLAUDE.md "Adding agent config fields" for the convention.
func TestAgentFieldSync(t *testing.T) {
	// Fields that exist on Agent but are NOT overridable via patch/override.
	// Add to this list with a comment explaining why.
	excluded := map[string]string{
		"Name":        "identity field, not overridable",
		"Description": "display field for MC session creation UI, not overridable via patch",
		// Provider-level fields: set during ResolveProvider, not typically
		// overridden per-rig. Agent-level overrides happen in the Agent
		// struct itself (which feeds into ResolveProvider).
		"Args":                   "provider field, set via ResolveProvider",
		"PromptMode":             "provider field, set via ResolveProvider",
		"PromptFlag":             "provider field, set via ResolveProvider",
		"ReadyDelayMs":           "provider field, set via ResolveProvider",
		"ReadyPromptPrefix":      "provider field, set via ResolveProvider",
		"ProcessNames":           "provider field, set via ResolveProvider",
		"EmitsPermissionWarning": "provider field, set via ResolveProvider",
		"WorkQuery":              "agent-specific, derived from name — not a patch concern",
		"SlingQuery":             "agent-specific, derived from name/pool — not a patch concern",
		"MaxActiveSessions":      "cap field, inherits from rig/workspace — not a patch concern",
		"MinActiveSessions":      "cap field, inherits from rig/workspace — not a patch concern",
		"ScaleCheck":             "agent-specific scaling, derived from pool config — not a patch concern",
		"SourceDir":              "runtime-only, set during pack/fragment loading",
		"Fallback":               "pack composition hint, not overridable at runtime",
		"PoolName":               "internal field set during pool expansion, not user-configurable",
		"Implicit":               "runtime-only, set during InjectImplicitAgents, not user-configurable",
		"SleepAfterIdleSource":   "runtime-only provenance, derived from the layer that set SleepAfterIdle",
		"DrainTimeout":           "scaling field, patched via PoolOverride.DrainTimeout",
		"OnBoot":                 "scaling field, patched via PoolOverride.OnBoot",
		"OnDeath":                "scaling field, patched via PoolOverride.OnDeath",
		"Namepool":               "agent-specific file path, not a patch concern",
		"NamepoolNames":          "runtime-only, loaded from Namepool file at config load time",
	}

	// Fields on AgentOverride/AgentPatch that don't map 1:1 to Agent fields.
	// "Agent" is the targeting key on AgentOverride, "EnvRemove" is a
	// remove-only modifier that has no Agent equivalent.
	patchOnly := map[string]bool{
		"Agent":                   true, // targeting key on AgentOverride
		"EnvRemove":               true, // remove modifier, no Agent field
		"PreStartAppend":          true, // append modifier, no Agent field
		"SessionSetupAppend":      true, // append modifier, no Agent field
		"SessionLiveAppend":       true, // append modifier, no Agent field
		"InstallAgentHooksAppend": true, // append modifier, no Agent field
		"InjectFragmentsAppend":   true, // append modifier, no Agent field
		"Pool":                    true, // legacy PoolOverride, maps to flat Agent fields via applyPoolOverride
	}

	agentFields := structFields(reflect.TypeOf(Agent{}))
	patchFields := structFields(reflect.TypeOf(AgentPatch{}))
	overrideFields := structFields(reflect.TypeOf(AgentOverride{}))

	// Remove excluded fields from agent set.
	var expected []string
	for _, f := range agentFields {
		if _, ok := excluded[f]; !ok {
			expected = append(expected, f)
		}
	}
	sort.Strings(expected)

	// Check AgentPatch has all expected fields.
	patchSet := toSet(patchFields)
	for _, k := range patchOnly {
		_ = k // just documenting
	}
	var missingPatch []string
	for _, f := range expected {
		if !patchSet[f] {
			missingPatch = append(missingPatch, f)
		}
	}
	if len(missingPatch) > 0 {
		t.Errorf("AgentPatch missing fields that exist on Agent: %v\n"+
			"Add them to AgentPatch or add to the excluded map with justification.", missingPatch)
	}

	// Check AgentOverride has all expected fields.
	overrideSet := toSet(overrideFields)
	var missingOverride []string
	for _, f := range expected {
		if !overrideSet[f] {
			missingOverride = append(missingOverride, f)
		}
	}
	if len(missingOverride) > 0 {
		t.Errorf("AgentOverride missing fields that exist on Agent: %v\n"+
			"Add them to AgentOverride or add to the excluded map with justification.", missingOverride)
	}

	// Check for extra fields on Patch/Override that aren't on Agent or patchOnly.
	agentSet := toSet(agentFields)
	for _, f := range patchFields {
		if !agentSet[f] && !patchOnly[f] {
			t.Errorf("AgentPatch has field %q not found on Agent or patchOnly exclusion list", f)
		}
	}
	for _, f := range overrideFields {
		if !agentSet[f] && !patchOnly[f] {
			t.Errorf("AgentOverride has field %q not found on Agent or patchOnly exclusion list", f)
		}
	}
}

func TestSessionSleepFieldSync(t *testing.T) {
	var expected []string
	cfgType := reflect.TypeOf(SessionSleepConfig{})
	for i := 0; i < cfgType.NumField(); i++ {
		field := cfgType.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := strings.Split(field.Tag.Get("toml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		expected = append(expected, tag)
	}
	sort.Strings(expected)

	fields := sessionSleepMergeFields(&City{}, &City{})
	got := make([]string, 0, len(fields))
	for _, field := range fields {
		got = append(got, field.key)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("session_sleep merge fields = %v, want %v", got, expected)
	}
}

// TestApplyAgentPatchCoversAllFields verifies that applyAgentPatchFields
// actually handles every field on AgentPatch. If a new field is added to
// AgentPatch but not wired into applyAgentPatchFields, this test catches it.
func TestApplyAgentPatchCoversAllFields(t *testing.T) {
	trueVal := true
	strVal := func(s string) *string { return &s }
	intVal := func(n int) *int { return &n }

	patch := AgentPatch{
		Dir:                     "target-dir",
		Name:                    "target-name",
		WorkDir:                 strVal(".gc/agents/worker"),
		Scope:                   strVal("city"),
		Suspended:               &trueVal,
		Attach:                  &trueVal,
		Pool:                    &PoolOverride{Min: intVal(2), Max: intVal(10), Check: strVal("echo 5"), OnDeath: strVal("echo dead"), OnBoot: strVal("echo boot")},
		Env:                     map[string]string{"KEY": "val"},
		PreStart:                []string{"pre-cmd"},
		PromptTemplate:          strVal("prompts/test.md"),
		Session:                 strVal("acp"),
		Provider:                strVal("claude"),
		StartCommand:            strVal("claude --dangerously"),
		Nudge:                   strVal("wake up"),
		IdleTimeout:             strVal("15m"),
		SleepAfterIdle:          strVal("30s"),
		InstallAgentHooks:       []string{"claude"},
		HooksInstalled:          &trueVal,
		SessionSetup:            []string{"setup-cmd"},
		SessionSetupScript:      strVal("scripts/setup.sh"),
		SessionLive:             []string{"live-cmd"},
		OverlayDir:              strVal("overlays/test"),
		DefaultSlingFormula:     strVal("mol-work"),
		InjectFragments:         []string{"frag1"},
		DependsOn:               []string{"other-agent"},
		ResumeCommand:           strVal("claude --resume {{.SessionKey}}"),
		WakeMode:                strVal("fresh"),
		PreStartAppend:          []string{"pre-append"},
		SessionSetupAppend:      []string{"setup-append"},
		SessionLiveAppend:       []string{"live-append"},
		InstallAgentHooksAppend: []string{"gemini"},
		InjectFragmentsAppend:   []string{"frag2"},
		EnvRemove:               []string{"REMOVE_ME"},
		MaxActiveSessions:       intVal(5),
		MinActiveSessions:       intVal(1),
		ScaleCheck:              strVal("echo 3"),
		OptionDefaults:          map[string]string{"model": "sonnet"},
		Lifecycle:               &LifecyclePatch{DrainPolicy: strVal("defer_until_idle"), IdleSignal: strVal("bead_activity"), GraceTimeout: strVal("5m")},
	}

	// Verify every AgentPatch field is set (non-zero).
	pv := reflect.ValueOf(patch)
	pt := pv.Type()
	for i := 0; i < pt.NumField(); i++ {
		f := pt.Field(i)
		if pv.Field(i).IsZero() {
			t.Errorf("AgentPatch field %q is zero in test data — add it to the test patch", f.Name)
		}
	}

	// Apply the patch to a zero-valued agent.
	agent := Agent{Env: map[string]string{"REMOVE_ME": "gone"}}
	applyAgentPatchFields(&agent, &patch)

	// Fields on AgentPatch that target the agent (Dir/Name are targeting keys,
	// not applied to the agent). EnvRemove removes keys. *Append modifiers
	// append to the base list set by the non-Append field.
	targeting := map[string]bool{"Dir": true, "Name": true}
	modifiers := map[string]bool{
		"EnvRemove":               true,
		"PreStartAppend":          true,
		"SessionSetupAppend":      true,
		"SessionLiveAppend":       true,
		"InstallAgentHooksAppend": true,
		"InjectFragmentsAppend":   true,
	}

	// Check that all non-targeting, non-modifier fields were applied.
	av := reflect.ValueOf(agent)
	at := av.Type()
	agentFieldByName := make(map[string]int, at.NumField())
	for i := 0; i < at.NumField(); i++ {
		agentFieldByName[at.Field(i).Name] = i
	}

	for i := 0; i < pt.NumField(); i++ {
		fname := pt.Field(i).Name
		if targeting[fname] || modifiers[fname] {
			continue
		}
		// Env, OptionDefaults, Pool, and Lifecycle are handled specially (not a direct field copy).
		if fname == "Env" || fname == "OptionDefaults" || fname == "Pool" || fname == "Lifecycle" {
			continue
		}
		idx, ok := agentFieldByName[fname]
		if !ok {
			continue // patchOnly field
		}
		if av.Field(idx).IsZero() {
			t.Errorf("applyAgentPatchFields did not apply field %q to Agent", fname)
		}
	}

	// Verify Lifecycle was merged.
	if agent.Lifecycle.DrainPolicy != "defer_until_idle" {
		t.Errorf("Lifecycle.DrainPolicy = %q, want %q", agent.Lifecycle.DrainPolicy, "defer_until_idle")
	}
	if agent.Lifecycle.IdleSignal != "bead_activity" {
		t.Errorf("Lifecycle.IdleSignal = %q, want %q", agent.Lifecycle.IdleSignal, "bead_activity")
	}
	if agent.Lifecycle.GraceTimeout != "5m" {
		t.Errorf("Lifecycle.GraceTimeout = %q, want %q", agent.Lifecycle.GraceTimeout, "5m")
	}
	// Verify Env was merged.
	if agent.Env["KEY"] != "val" {
		t.Errorf("Env[KEY] = %q, want %q", agent.Env["KEY"], "val")
	}
	// Verify OptionDefaults was merged.
	if agent.OptionDefaults["model"] != "sonnet" {
		t.Errorf("OptionDefaults[model] = %q, want %q", agent.OptionDefaults["model"], "sonnet")
	}
	// Verify EnvRemove worked.
	if _, exists := agent.Env["REMOVE_ME"]; exists {
		t.Error("EnvRemove did not remove REMOVE_ME from Env")
	}
	// Verify scaling was applied (via PoolOverride).
	if agent.MinActiveSessions == nil || *agent.MinActiveSessions != 2 || agent.MaxActiveSessions == nil || *agent.MaxActiveSessions != 10 {
		t.Errorf("Scaling not applied correctly: min=%v max=%v", agent.MinActiveSessions, agent.MaxActiveSessions)
	}
	// Verify append modifiers extended the lists (not replaced).
	if len(agent.PreStart) != 2 || agent.PreStart[1] != "pre-append" {
		t.Errorf("PreStartAppend not applied: %v", agent.PreStart)
	}
	if len(agent.SessionSetup) != 2 || agent.SessionSetup[1] != "setup-append" {
		t.Errorf("SessionSetupAppend not applied: %v", agent.SessionSetup)
	}
	if len(agent.SessionLive) != 2 || agent.SessionLive[1] != "live-append" {
		t.Errorf("SessionLiveAppend not applied: %v", agent.SessionLive)
	}
	if len(agent.InstallAgentHooks) != 2 || agent.InstallAgentHooks[1] != "gemini" {
		t.Errorf("InstallAgentHooksAppend not applied: %v", agent.InstallAgentHooks)
	}
	if len(agent.InjectFragments) != 2 || agent.InjectFragments[1] != "frag2" {
		t.Errorf("InjectFragmentsAppend not applied: %v", agent.InjectFragments)
	}
}

// TestApplyAgentOverrideCoversAllFields verifies that applyAgentOverride
// actually handles every field on AgentOverride. Same approach as the patch
// test: set every field, apply, check no Agent field is left at zero.
func TestApplyAgentOverrideCoversAllFields(t *testing.T) {
	trueVal := true
	strVal := func(s string) *string { return &s }
	intVal := func(n int) *int { return &n }

	override := AgentOverride{
		Agent:                   "target",
		Dir:                     strVal("new-dir"),
		WorkDir:                 strVal(".gc/agents/target"),
		Scope:                   strVal("city"),
		Suspended:               &trueVal,
		Attach:                  &trueVal,
		Pool:                    &PoolOverride{Min: intVal(2), Max: intVal(10), Check: strVal("echo 5"), OnDeath: strVal("echo dead"), OnBoot: strVal("echo boot")},
		Env:                     map[string]string{"KEY": "val"},
		EnvRemove:               []string{"REMOVE_ME"},
		PreStart:                []string{"pre-cmd"},
		PromptTemplate:          strVal("prompts/test.md"),
		Session:                 strVal("acp"),
		Provider:                strVal("claude"),
		StartCommand:            strVal("claude --dangerously"),
		Nudge:                   strVal("wake up"),
		IdleTimeout:             strVal("15m"),
		SleepAfterIdle:          strVal("30s"),
		InstallAgentHooks:       []string{"claude"},
		HooksInstalled:          &trueVal,
		SessionSetup:            []string{"setup-cmd"},
		SessionSetupScript:      strVal("scripts/setup.sh"),
		SessionLive:             []string{"live-cmd"},
		OverlayDir:              strVal("overlays/test"),
		DefaultSlingFormula:     strVal("mol-work"),
		InjectFragments:         []string{"frag1"},
		DependsOn:               []string{"other-agent"},
		ResumeCommand:           strVal("claude --resume {{.SessionKey}}"),
		WakeMode:                strVal("fresh"),
		PreStartAppend:          []string{"pre-append"},
		SessionSetupAppend:      []string{"setup-append"},
		SessionLiveAppend:       []string{"live-append"},
		InstallAgentHooksAppend: []string{"gemini"},
		InjectFragmentsAppend:   []string{"frag2"},
		MaxActiveSessions:       intVal(5),
		MinActiveSessions:       intVal(1),
		ScaleCheck:              strVal("echo 3"),
		OptionDefaults:          map[string]string{"model": "sonnet"},
		Lifecycle:               &LifecyclePatch{DrainPolicy: strVal("defer_until_idle"), IdleSignal: strVal("bead_activity"), GraceTimeout: strVal("5m")},
	}

	// Verify every AgentOverride field is set (non-zero).
	ov := reflect.ValueOf(override)
	ot := ov.Type()
	for i := 0; i < ot.NumField(); i++ {
		f := ot.Field(i)
		if ov.Field(i).IsZero() {
			t.Errorf("AgentOverride field %q is zero in test data — add it to the test override", f.Name)
		}
	}

	// Apply the override to a zero-valued agent.
	agent := Agent{Env: map[string]string{"REMOVE_ME": "gone"}}
	applyAgentOverride(&agent, &override)

	// "Agent" is the targeting key, not applied to the agent.
	targeting := map[string]bool{"Agent": true}
	modifiers := map[string]bool{
		"EnvRemove":               true,
		"PreStartAppend":          true,
		"SessionSetupAppend":      true,
		"SessionLiveAppend":       true,
		"InstallAgentHooksAppend": true,
		"InjectFragmentsAppend":   true,
	}

	av := reflect.ValueOf(agent)
	at := av.Type()
	agentFieldByName := make(map[string]int, at.NumField())
	for i := 0; i < at.NumField(); i++ {
		agentFieldByName[at.Field(i).Name] = i
	}

	for i := 0; i < ot.NumField(); i++ {
		fname := ot.Field(i).Name
		if targeting[fname] || modifiers[fname] {
			continue
		}
		if fname == "Env" || fname == "OptionDefaults" || fname == "Pool" || fname == "Lifecycle" {
			continue
		}
		idx, ok := agentFieldByName[fname]
		if !ok {
			continue
		}
		if av.Field(idx).IsZero() {
			t.Errorf("applyAgentOverride did not apply field %q to Agent", fname)
		}
	}

	// Verify Lifecycle was merged.
	if agent.Lifecycle.DrainPolicy != "defer_until_idle" {
		t.Errorf("Lifecycle.DrainPolicy = %q, want %q", agent.Lifecycle.DrainPolicy, "defer_until_idle")
	}
	// Verify Env was merged.
	if agent.Env["KEY"] != "val" {
		t.Errorf("Env[KEY] = %q, want %q", agent.Env["KEY"], "val")
	}
	if _, exists := agent.Env["REMOVE_ME"]; exists {
		t.Error("EnvRemove did not remove REMOVE_ME from Env")
	}
	// Verify OptionDefaults was merged.
	if agent.OptionDefaults["model"] != "sonnet" {
		t.Errorf("OptionDefaults[model] = %q, want %q", agent.OptionDefaults["model"], "sonnet")
	}
	if agent.MinActiveSessions == nil || *agent.MinActiveSessions != 2 || agent.MaxActiveSessions == nil || *agent.MaxActiveSessions != 10 {
		t.Errorf("Scaling not applied correctly: min=%v max=%v", agent.MinActiveSessions, agent.MaxActiveSessions)
	}
}

func structFields(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		names = append(names, t.Field(i).Name)
	}
	return names
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
