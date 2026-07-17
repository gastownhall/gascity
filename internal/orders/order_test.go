package orders

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	data := []byte(`
[order]
description = "Generate daily digest"
formula = "mol-digest-generate"
trigger = "cooldown"
interval = "24h"
pool = "dog"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Formula != "mol-digest-generate" {
		t.Errorf("Formula = %q, want %q", a.Formula, "mol-digest-generate")
	}
	if a.Trigger != "cooldown" {
		t.Errorf("Trigger = %q, want %q", a.Trigger, "cooldown")
	}
	if a.Interval != "24h" {
		t.Errorf("Interval = %q, want %q", a.Interval, "24h")
	}
	if a.Pool != "dog" {
		t.Errorf("Pool = %q, want %q", a.Pool, "dog")
	}
	if a.Description != "Generate daily digest" {
		t.Errorf("Description = %q, want %q", a.Description, "Generate daily digest")
	}
}

func TestParseEnabledDefault(t *testing.T) {
	data := []byte(`
[order]
formula = "test"
trigger = "manual"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !a.IsEnabled() {
		t.Error("IsEnabled() = false, want true (default)")
	}
}

func TestParseEnabledExplicitFalse(t *testing.T) {
	data := []byte(`
[order]
formula = "test"
trigger = "manual"
enabled = false
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.IsEnabled() {
		t.Error("IsEnabled() = true, want false")
	}
}

func TestParseInvalid(t *testing.T) {
	_, err := Parse([]byte(`not valid toml {{{`))
	if err == nil {
		t.Fatal("Parse should fail on invalid TOML")
	}
}

func TestParseIdempotent(t *testing.T) {
	on, err := Parse([]byte("[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"1m\"\nidempotent = true\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !on.Idempotent {
		t.Error("Idempotent = false, want true")
	}
	off, err := Parse([]byte("[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"1m\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if off.Idempotent {
		t.Error("Idempotent = true, want false (default)")
	}
}

func TestValidateCooldown(t *testing.T) {
	a := Order{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h"}
	if err := Validate(a); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateCooldownMissingInterval(t *testing.T) {
	a := Order{Name: "digest", Formula: "mol-digest", Trigger: "cooldown"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: cooldown without interval")
	}
}

func TestValidateCooldownBadInterval(t *testing.T) {
	a := Order{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "not-a-duration"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: invalid interval")
	}
}

func TestValidateCron(t *testing.T) {
	a := Order{Name: "cleanup", Formula: "mol-cleanup", Trigger: "cron", Schedule: "0 3 * * *"}
	if err := Validate(a); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateCronMissingSchedule(t *testing.T) {
	a := Order{Name: "cleanup", Formula: "mol-cleanup", Trigger: "cron"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: cron without schedule")
	}
}

func TestValidateCondition(t *testing.T) {
	a := Order{Name: "check", Formula: "mol-check", Trigger: "condition", Check: "test -f /tmp/flag"}
	if err := Validate(a); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateConditionMissingCheck(t *testing.T) {
	a := Order{Name: "check", Formula: "mol-check", Trigger: "condition"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: condition without check")
	}
}

func TestValidateManual(t *testing.T) {
	a := Order{Name: "deploy", Formula: "mol-deploy", Trigger: "manual"}
	if err := Validate(a); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateMissingFormulaAndExec(t *testing.T) {
	a := Order{Name: "bad", Trigger: "manual"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: missing formula and exec")
	}
}

func TestValidateExecOrder(t *testing.T) {
	a := Order{Name: "poller", Exec: "scripts/poll.sh", Trigger: "cooldown", Interval: "2m"}
	if err := Validate(a); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateExecAndFormulaMutuallyExclusive(t *testing.T) {
	a := Order{Name: "both", Formula: "mol-x", Exec: "scripts/x.sh", Trigger: "manual"}
	err := Validate(a)
	if err == nil {
		t.Error("Validate should fail: formula and exec both set")
	}
}

func TestValidateExecWithPool(t *testing.T) {
	a := Order{Name: "bad", Exec: "scripts/x.sh", Trigger: "manual", Pool: "worker"}
	err := Validate(a)
	if err == nil {
		t.Error("Validate should fail: exec with pool")
	}
}

func TestValidateFormulaWithEnv(t *testing.T) {
	a := Order{Name: "bad", Formula: "mol-x", Trigger: "manual", Env: map[string]string{"CUSTOM_ORDER_FLAG": "enabled"}}
	err := Validate(a)
	if err == nil {
		t.Fatal("Validate should fail: formula order with env")
	}
	if !strings.Contains(err.Error(), "env") {
		t.Fatalf("Validate error = %q, want env diagnostic", err)
	}
}

func TestValidateEnvKeyShape(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "contains equals", key: "BAD=KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Order{Name: "bad", Exec: "scripts/x.sh", Trigger: "manual", Env: map[string]string{tt.key: "value"}}
			err := Validate(a)
			if err == nil {
				t.Fatal("Validate should fail for invalid env key")
			}
			if !strings.Contains(err.Error(), "env") {
				t.Fatalf("Validate error = %q, want env diagnostic", err)
			}
		})
	}
}

func TestValidateTimeout(t *testing.T) {
	a := Order{Name: "t", Formula: "mol-t", Trigger: "manual", Timeout: "90s"}
	if err := Validate(a); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateTimeoutInvalid(t *testing.T) {
	a := Order{Name: "t", Formula: "mol-t", Trigger: "manual", Timeout: "not-a-duration"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: invalid timeout")
	}
}

func TestIsExec(t *testing.T) {
	exec := Order{Name: "e", Exec: "scripts/x.sh"}
	if !exec.IsExec() {
		t.Error("IsExec() = false, want true")
	}
	formula := Order{Name: "f", Formula: "mol-f"}
	if formula.IsExec() {
		t.Error("IsExec() = true, want false")
	}
}

func TestTimeoutOrDefault(t *testing.T) {
	tests := []struct {
		name string
		a    Order
		want time.Duration
	}{
		{"exec default", Order{Exec: "x.sh"}, 300 * time.Second},
		{"formula default", Order{Formula: "mol-x"}, 30 * time.Second},
		{"custom timeout", Order{Exec: "x.sh", Timeout: "90s"}, 90 * time.Second},
		{"invalid timeout falls back", Order{Exec: "x.sh", Timeout: "bad"}, 300 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.TimeoutOrDefault()
			if got != tt.want {
				t.Errorf("TimeoutOrDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseExecOrder(t *testing.T) {
	data := []byte(`
[order]
description = "Poll wasteland"
exec = "$ORDER_DIR/scripts/poll.sh"
trigger = "cooldown"
interval = "2m"
timeout = "90s"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Exec != "$ORDER_DIR/scripts/poll.sh" {
		t.Errorf("Exec = %q, want %q", a.Exec, "$ORDER_DIR/scripts/poll.sh")
	}
	if a.Formula != "" {
		t.Errorf("Formula = %q, want empty", a.Formula)
	}
	if a.Timeout != "90s" {
		t.Errorf("Timeout = %q, want %q", a.Timeout, "90s")
	}
}

func TestValidateMissingTrigger(t *testing.T) {
	a := Order{Name: "bad", Formula: "mol-bad"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: missing trigger")
	}
}

func TestValidateUnknownTrigger(t *testing.T) {
	a := Order{Name: "bad", Formula: "mol-bad", Trigger: "random"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: unknown trigger type")
	}
}

func TestValidateEvent(t *testing.T) {
	a := Order{Name: "convoy-check", Formula: "mol-convoy-check", Trigger: "event", On: "bead.closed"}
	if err := Validate(a); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestScopedNameCityLevel(t *testing.T) {
	a := Order{Name: "dolt-health"}
	if got := a.ScopedName(); got != "dolt-health" {
		t.Errorf("ScopedName() = %q, want %q", got, "dolt-health")
	}
}

func TestScopedNameRigLevel(t *testing.T) {
	a := Order{Name: "dolt-health", Rig: "demo-repo"}
	want := "dolt-health:rig:demo-repo"
	if got := a.ScopedName(); got != want {
		t.Errorf("ScopedName() = %q, want %q", got, want)
	}
}

func TestValidateEventMissingOn(t *testing.T) {
	a := Order{Name: "convoy-check", Formula: "mol-convoy-check", Trigger: "event"}
	if err := Validate(a); err == nil {
		t.Error("Validate should fail: event without on")
	}
}

func TestParseEventOrder(t *testing.T) {
	data := []byte(`
[order]
description = "Auto-close convoys where all children are closed"
formula = "mol-convoy-check"
trigger = "event"
on = "bead.closed"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Trigger != "event" {
		t.Errorf("Trigger = %q, want %q", a.Trigger, "event")
	}
	if a.On != "bead.closed" {
		t.Errorf("On = %q, want %q", a.On, "bead.closed")
	}
	if a.Formula != "mol-convoy-check" {
		t.Errorf("Formula = %q, want %q", a.Formula, "mol-convoy-check")
	}
}

func TestParseLegacyGateAlias(t *testing.T) {
	data := []byte(`
[order]
formula = "mol-digest-generate"
gate = "cooldown"
interval = "24h"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Trigger != "cooldown" {
		t.Fatalf("Trigger = %q, want %q", a.Trigger, "cooldown")
	}
}

func TestParseTriggerWinsOverLegacyGate(t *testing.T) {
	data := []byte(`
[order]
formula = "mol-digest-generate"
trigger = "cron"
gate = "cooldown"
schedule = "0 3 * * *"
interval = "24h"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Trigger != "cron" {
		t.Fatalf("Trigger = %q, want %q", a.Trigger, "cron")
	}
}

func TestParseEnv(t *testing.T) {
	data := []byte(`
[order]
exec = "scripts/doctor.sh"
trigger = "cooldown"
interval = "5m"

[order.env]
GC_DOCTOR_LATENCY_WARN_S = "3"
GC_JSONL_SPIKE_THRESHOLD = "30"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Env["GC_DOCTOR_LATENCY_WARN_S"] != "3" {
		t.Errorf("Env[GC_DOCTOR_LATENCY_WARN_S] = %q, want %q", a.Env["GC_DOCTOR_LATENCY_WARN_S"], "3")
	}
	if a.Env["GC_JSONL_SPIKE_THRESHOLD"] != "30" {
		t.Errorf("Env[GC_JSONL_SPIKE_THRESHOLD] = %q, want %q", a.Env["GC_JSONL_SPIKE_THRESHOLD"], "30")
	}
}

func TestParseEnvAbsent(t *testing.T) {
	data := []byte(`
[order]
formula = "mol-test"
trigger = "manual"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Env) != 0 {
		t.Errorf("Env = %v, want empty when absent", a.Env)
	}
}

func TestParseScope(t *testing.T) {
	data := []byte(`
[order]
scope = "city"
exec = "scripts/sweep.sh"
trigger = "cooldown"
interval = "5m"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Scope != "city" {
		t.Errorf("Scope = %q, want %q", a.Scope, "city")
	}
	if !a.IsCityScoped() {
		t.Error("IsCityScoped() = false, want true for scope=city")
	}
}

func TestParseScopeDefaultsToRig(t *testing.T) {
	data := []byte(`
[order]
exec = "scripts/health.sh"
trigger = "cooldown"
interval = "5m"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Scope != "" {
		t.Errorf("Scope = %q, want empty (rig default)", a.Scope)
	}
	if a.IsCityScoped() {
		t.Error("IsCityScoped() = true, want false for unscoped order")
	}
}

func TestValidateRejectsUnknownScope(t *testing.T) {
	a := Order{
		Name:     "bad-scope",
		Exec:     "scripts/x.sh",
		Trigger:  "cooldown",
		Interval: "5m",
		Scope:    "global",
	}
	err := Validate(a)
	if err == nil {
		t.Fatal("Validate succeeded, want unknown-scope rejection")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Fatalf("Validate error = %q, want scope context", err.Error())
	}
}

func TestValidateAcceptsCityAndRigScope(t *testing.T) {
	for _, scope := range []string{"", "city", "rig"} {
		a := Order{
			Name:     "scoped",
			Exec:     "scripts/x.sh",
			Trigger:  "cooldown",
			Interval: "5m",
			Scope:    scope,
		}
		if err := Validate(a); err != nil {
			t.Errorf("Validate(scope=%q) = %v, want nil", scope, err)
		}
	}
}

func TestParseOrderParams(t *testing.T) {
	data := []byte(`
[order]
formula = "pr-review"
trigger = "manual"

[order.params]
repo = { required = true }
pr = { required = true }
note = {}
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Params) != 3 {
		t.Fatalf("len(Params) = %d, want 3", len(a.Params))
	}
	if !a.Params["repo"].Required {
		t.Fatal("Params[repo].Required = false, want true")
	}
	if a.Params["note"].Required {
		t.Fatal("Params[note].Required = true, want false")
	}
}

func TestValidateRequiredParams(t *testing.T) {
	a := Order{
		Name:    "pr-review",
		Formula: "pr-review",
		Trigger: "manual",
		Params: map[string]OrderParam{
			"repo": {Required: true},
			"pr":   {Required: true},
			"note": {Required: false},
		},
	}

	if err := ValidateRequiredParams(a, map[string]string{"repo": "octo/demo", "pr": "1"}); err != nil {
		t.Fatalf("ValidateRequiredParams with all required present = %v, want nil", err)
	}

	// Optional param may be omitted.
	if err := ValidateRequiredParams(a, map[string]string{"repo": "octo/demo", "pr": "1", "extra": "ignored"}); err != nil {
		t.Fatalf("ValidateRequiredParams with optional omitted = %v, want nil", err)
	}

	err := ValidateRequiredParams(a, map[string]string{"repo": "octo/demo"})
	if err == nil {
		t.Fatal("ValidateRequiredParams with missing pr = nil, want error")
	}
	if !strings.Contains(err.Error(), "pr") {
		t.Fatalf("error = %q, want it to name missing param pr", err.Error())
	}

	// A present-but-empty value counts as MISSING: webhook arg extraction inserts
	// the key even when the payload path resolved to "", so a required param that
	// rendered empty must not be treated as supplied (else the order fires with an
	// empty required value).
	emptyErr := ValidateRequiredParams(a, map[string]string{"repo": "octo/demo", "pr": ""})
	if emptyErr == nil {
		t.Fatal("ValidateRequiredParams with empty-but-present pr = nil, want error (empty required value is not supplied)")
	}
	if !strings.Contains(emptyErr.Error(), "pr") {
		t.Fatalf("error = %q, want it to name the empty required param pr", emptyErr.Error())
	}

	// A whitespace-only value is likewise treated as missing.
	if err := ValidateRequiredParams(a, map[string]string{"repo": "octo/demo", "pr": "   "}); err == nil {
		t.Fatal("ValidateRequiredParams with whitespace-only pr = nil, want error")
	}
}

func TestParseCronTZ(t *testing.T) {
	data := []byte(`
[order]
formula = "mol-digest-generate"
trigger = "cron"
schedule = "30 19 * * *"
tz = "America/New_York"
`)
	a, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.TZ != "America/New_York" {
		t.Errorf("TZ = %q, want %q", a.TZ, "America/New_York")
	}
}

func TestValidateCronTZ(t *testing.T) {
	a := Order{Name: "digest", Formula: "mol-digest", Trigger: "cron", Schedule: "30 19 * * *", TZ: "America/New_York"}
	if err := Validate(a); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// A misspelled zone must fail order load loudly — a silent fallback would
// move the order's schedule onto a different wall clock.
func TestValidateCronBadTZ(t *testing.T) {
	a := Order{Name: "digest", Formula: "mol-digest", Trigger: "cron", Schedule: "30 19 * * *", TZ: "America/New_Yrok"}
	err := Validate(a)
	if err == nil {
		t.Fatal("Validate should fail: bad tz")
	}
	if !strings.Contains(err.Error(), `invalid tz "America/New_Yrok"`) {
		t.Errorf("error = %q, want it to name the invalid tz", err)
	}
}
