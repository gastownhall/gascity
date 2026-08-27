//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestAdoptionLatencyReportNearestRankBoundaries(t *testing.T) {
	samples := make([]adoptionLatencySample, 20)
	for i := range samples {
		samples[i] = readyAdoptionLatencySample(i+1, time.Duration(i+1)*time.Millisecond)
	}
	observed := time.Second + time.Millisecond
	samples[0].StartingBeadStore = &observed
	report, err := buildAdoptionLatencyReport(adoptionLatencyProvenanceForTest(), adoptionLatencyRunState{Requested: 20, Warmup: readyAdoptionLatencySample(0, time.Millisecond), Samples: samples})
	if err != nil {
		t.Fatal(err)
	}
	got := []time.Duration{report.Latency.P50, report.Latency.P95, report.Latency.P99, report.Latency.Max}
	want := []time.Duration{10 * time.Millisecond, 19 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond}
	store := report.PhaseStats[len(report.PhaseStats)-1]
	if !report.OK || !slices.Equal(got, want) || store.ObservedCount != 1 || store.CensoredCount != 19 ||
		store.CensorThreshold != time.Second || !store.PercentilesObservedOnly {
		t.Fatalf("latency=%v store=%+v, want %v and 1/19 censored stats", got, store, want)
	}
}

func TestAdoptionLatencyReportRequiresExactBenchmarkProfile(t *testing.T) {
	validProfile := adoptionLatencyBenchmarkProfileV1()
	validRun := adoptionLatencyRunState{
		Requested: 1,
		Warmup:    readyAdoptionLatencySample(0, time.Millisecond),
		Samples:   []adoptionLatencySample{readyAdoptionLatencySample(1, time.Millisecond)},
	}
	base := adoptionLatencyProvenanceForTest()
	report, err := buildAdoptionLatencyReport(base, validRun)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Provenance.BenchmarkProfile != validProfile {
		t.Fatalf("valid benchmark profile produced report %+v", report)
	}

	for name, mutate := range map[string]func(*adoptionLatencyBenchmarkProfile){
		"missing":            func(profile *adoptionLatencyBenchmarkProfile) { *profile = adoptionLatencyBenchmarkProfile{} },
		"version":            func(profile *adoptionLatencyBenchmarkProfile) { profile.Version = "wrong" },
		"beads provider":     func(profile *adoptionLatencyBenchmarkProfile) { profile.BeadsProvider = "wrong" },
		"shadow mode":        func(profile *adoptionLatencyBenchmarkProfile) { profile.ShadowMode = "wrong" },
		"dispatcher mode":    func(profile *adoptionLatencyBenchmarkProfile) { profile.DispatcherMode = "wrong" },
		"reconciler mode":    func(profile *adoptionLatencyBenchmarkProfile) { profile.ReconcilerMode = "wrong" },
		"preserve on signal": func(profile *adoptionLatencyBenchmarkProfile) { profile.PreserveSessionsOnSignal = false },
		"measurement method": func(profile *adoptionLatencyBenchmarkProfile) { profile.MeasurementMethod = "wrong" },
	} {
		provenance := base
		mutate(&provenance.BenchmarkProfile)
		report, err := buildAdoptionLatencyReport(provenance, validRun)
		if err != nil {
			t.Fatalf("%s profile: %v", name, err)
		}
		if report.OK {
			t.Errorf("%s profile produced ok=true", name)
		}
	}
}

func TestAdoptionLatencyReportRetainsAndExcludesNonReadySamples(t *testing.T) {
	run := adoptionLatencyRunState{Requested: 4, Warmup: readyAdoptionLatencySample(0, time.Millisecond), Samples: []adoptionLatencySample{
		readyAdoptionLatencySample(1, time.Millisecond),
		{Index: 2, Outcome: adoptionLatencyOutcomeIncomplete, Error: "deadline"},
		{Index: 3, Outcome: adoptionLatencyOutcomeError, Error: "failed"},
		{Index: 4, Outcome: adoptionLatencyOutcomeNotAttempted, Error: "unsafe"},
	}}
	report, err := buildAdoptionLatencyReport(adoptionLatencyProvenanceForTest(), run)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(report)
	second, _ := json.Marshal(report)
	if !bytes.Equal(first, second) || report.Latency.Count != 1 || len(report.Samples) != 4 || report.OK {
		t.Fatalf("report accounting = %+v; deterministic=%t", report, bytes.Equal(first, second))
	}
	for name, breakSample := range map[string]func(*adoptionLatencySample){
		"missing": func(s *adoptionLatencySample) { s.Phases = s.Phases[:5] },
		"extra":   func(s *adoptionLatencySample) { s.Phases = append(s.Phases, s.Phases[0]) },
		"order":   func(s *adoptionLatencySample) { s.Phases[0], s.Phases[1] = s.Phases[1], s.Phases[0] },
	} {
		sample := readyAdoptionLatencySample(1, time.Millisecond)
		breakSample(&sample)
		if _, err := buildAdoptionLatencyReport(adoptionLatencyProvenanceForTest(), adoptionLatencyRunState{Requested: 1, Warmup: run.Warmup, Samples: []adoptionLatencySample{sample}}); err == nil {
			t.Errorf("%s ready sample was accepted", name)
		}
		if report, err := buildAdoptionLatencyReport(adoptionLatencyProvenanceForTest(), adoptionLatencyRunState{Warmup: sample}); err != nil || report.OK {
			t.Errorf("%s ready warmup was accepted (err=%v)", name, err)
		}
	}
	base := adoptionLatencyProvenanceForTest()
	rejectMissing := func(name string, p adoptionLatencyProvenance) {
		report, err := buildAdoptionLatencyReport(p, adoptionLatencyRunState{Requested: 1, Warmup: readyAdoptionLatencySample(0, time.Millisecond), Samples: []adoptionLatencySample{readyAdoptionLatencySample(1, time.Millisecond)}})
		if err != nil || report.OK {
			t.Errorf("missing provenance %s produced ok=%t, err=%v", name, report.OK, err)
		}
	}
	for i := range base.Binaries {
		for _, field := range []string{"Name", "Path", "SHA256", "Version"} {
			p := base
			p.Binaries = slices.Clone(base.Binaries)
			reflect.ValueOf(&p.Binaries[i]).Elem().FieldByName(field).SetString("")
			rejectMissing(fmt.Sprintf("binary %d %s", i, field), p)
		}
	}
	for _, field := range []string{"GCCommit", "RuntimeProvider", "RuntimeIdentity", "HostOS", "HostArch", "CPUCount"} {
		p := base
		value := reflect.ValueOf(&p).Elem().FieldByName(field)
		value.Set(reflect.Zero(value.Type()))
		rejectMissing(field, p)
	}
	p := base
	p.RuntimeProvider = "hybrid"
	rejectMissing("hybrid runtime", p)
}

func TestAdoptionLatencyPerfRejectsGCOverride(t *testing.T) {
	t.Setenv(integrationGCBinaryEnv, "/tmp/gc")
	if err := adoptionLatencyPerfGCOverrideError(); err == nil || !strings.Contains(err.Error(), integrationGCBinaryEnv) {
		t.Fatalf("override rejection = %v", err)
	}
}

func TestAdoptionLatencyPerfInitialDeadlineReportsLastError(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Time{})
	cancel()
	if err := adoptionLatencyPerfWaitInitialReady(ctx, &adoptionLatencyPerfHarness{}); err == nil || !strings.Contains(err.Error(), "context deadline exceeded") || !strings.Contains(err.Error(), "last=") {
		t.Fatalf("expired initial readiness = %v", err)
	}
}

func TestAdoptionLatencyPerfRejectsUnsafeWitnesses(t *testing.T) {
	for name, session := range map[string]adoptionLatencyPerfSession{"active": {State: "active", Running: true}, "closed": {State: "closed", Running: true}, "suspended": {State: "suspended", Running: true}, "stopped": {State: "active"}} {
		if got, want := session.ready(), name == "active"; got != want {
			t.Errorf("%s session ready = %t, want %t", name, got, want)
		}
	}
	if err := adoptionLatencyPerfSignalSupervisor(0); err == nil {
		t.Error("zero supervisor PID was accepted")
	}
	for name, status := range map[string]string{"owned": `{"running":true,"pid":42}`, "zero": `{"running":true,"pid":0}`, "other": `{"running":true,"pid":41}`, "stopped": `{"running":false,"pid":42}`, "invalid": "{"} {
		if got, want := isolatedSupervisorStatusOwnsPID(status, 42), name == "owned"; got != want {
			t.Errorf("%s status ownership = %t, want %t", name, got, want)
		}
	}
}

func adoptionLatencyPerfGCOverrideError() error {
	if strings.TrimSpace(os.Getenv(integrationGCBinaryEnv)) == "" {
		return nil
	}
	return fmt.Errorf("%s is unsupported for the adoption latency benchmark; unset it so the benchmark builds, invokes, and hashes the same gc binary", integrationGCBinaryEnv)
}

func adoptionLatencyPerfSignalSupervisor(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("tracked test-owned supervisor PID must be positive, got %d", pid)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return fmt.Errorf("tracked test-owned supervisor PID %d is not alive: %w", pid, err)
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}

func TestParseAdoptionStartupPhases(t *testing.T) {
	var lines []string
	for _, name := range adoptionLatencyRequiredStartupPhases {
		lines = append(lines, "gc: startup phase="+name+" elapsed=1ms")
	}
	valid := strings.Join(lines, "\n")
	phases, store, err := parseAdoptionStartupPhases([]byte(valid))
	if err != nil || len(phases) != 6 || store != nil {
		t.Fatalf("valid parse = %+v, %v, %v", phases, store, err)
	}
	for name, log := range map[string]string{"malformed": strings.Replace(valid, "1ms", "nope", 1), "duplicate": valid + "\n" + lines[0], "missing": strings.Join(lines[1:], "\n")} {
		if _, _, err := parseAdoptionStartupPhases([]byte(log)); err == nil {
			t.Errorf("%s startup phases were accepted", name)
		}
	}
}

func readyAdoptionLatencySample(index int, duration time.Duration) adoptionLatencySample {
	phases := make([]adoptionLatencyPhase, len(adoptionLatencyRequiredStartupPhases))
	for i, name := range adoptionLatencyRequiredStartupPhases {
		phases[i] = adoptionLatencyPhase{Name: name, Duration: time.Millisecond}
	}
	return adoptionLatencySample{Index: index, Outcome: adoptionLatencyOutcomeReady, Duration: duration, Phases: phases}
}

type adoptionLatencyPerfSession struct {
	ID, Template, SessionName, State string
	Running                          bool
}
type adoptionLatencyPerfHarness struct {
	env                             []string
	gcHome, cityDir, cityName, tmux string
	session                         adoptionLatencyPerfSession
	client                          *genclient.ClientWithResponses
	pid                             int
}

func (s adoptionLatencyPerfSession) ready() bool { return s.State == "active" && s.Running }
func TestSupervisorPreserveAdoptionLatencyExactBinary(t *testing.T) {
	if os.Getenv("GC_RUN_ADOPTION_PERF") != "1" {
		t.Skip("set GC_RUN_ADOPTION_PERF=1 to run")
	}
	reportPath := strings.TrimSpace(os.Getenv("GC_ADOPTION_PERF_REPORT"))
	if !filepath.IsAbs(reportPath) {
		t.Fatal("GC_ADOPTION_PERF_REPORT must be an absolute path")
	}
	provenance := adoptionLatencyProvenance{Error: "setup not attempted", BenchmarkProfile: adoptionLatencyBenchmarkProfileV1()}
	run := adoptionLatencyRunState{Warmup: adoptionLatencySample{Outcome: adoptionLatencyOutcomeNotAttempted, Error: "setup not attempted"}, FatalError: "setup not attempted"}
	defer func() {
		if err := writeAdoptionLatencyReport(reportPath, provenance, run); err != nil {
			t.Errorf("publish report: %v", err)
		}
	}()
	if err := adoptionLatencyPerfGCOverrideError(); err != nil {
		run.FatalError = err.Error()
		t.Fatal(err)
	}
	requireDoltIntegration(t)
	maxSamples, samples := 30, 30
	if raw := strings.TrimSpace(os.Getenv("GC_ADOPTION_PERF_SAMPLES")); raw != "" {
		var err error
		if samples, err = strconv.Atoi(raw); err != nil || samples < 1 || samples > maxSamples {
			run.FatalError = fmt.Sprintf("GC_ADOPTION_PERF_SAMPLES must be an integer from 1 through %d", maxSamples)
			provenance.Error = run.FatalError
			t.Fatal(run.FatalError)
		}
	}
	run.Requested = samples
	for i := 1; i <= samples; i++ {
		run.Samples = append(run.Samples, adoptionLatencySample{Index: i, Outcome: adoptionLatencyOutcomeNotAttempted, Error: "journey ended before sample"})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 32*time.Minute)
	defer cancel()
	if err := adoptionLatencyPerfRun(t, ctx, &provenance, &run); err != nil {
		run.FatalError = err.Error()
		t.Fatal(err)
	}
}

func adoptionLatencyPerfRun(t *testing.T, ctx context.Context, p *adoptionLatencyProvenance, run *adoptionLatencyRunState) error {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported host %s", runtime.GOOS)
	}
	env := replaceEnv(newIsolatedCommandEnv(t, true), "GC_SESSION", "tmux")
	gcHome := parseEnvList(env)["GC_HOME"]
	if out, err := adoptionLatencyPerfCommand(ctx, "", env, 15*time.Second, gcBinary, "supervisor", "stop", "--wait"); err != nil {
		return fmt.Errorf("stop bootstrap supervisor: %w: %s", err, strings.TrimSpace(out))
	}
	profile := adoptionLatencyBenchmarkProfileV1()
	preserveOnSignal := "0"
	if profile.PreserveSessionsOnSignal {
		preserveOnSignal = "1"
	}
	env = replaceEnv(env, "GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL", preserveOnSignal)
	cityDir, port, err := adoptionLatencyPerfPrepareCity(t, env, gcHome)
	if err != nil {
		return err
	}
	pid := startIsolatedSupervisor(t, env, gcHome)
	for _, command := range [][]string{
		{"init", "--no-start", "--skip-provider-readiness", "--file", filepath.Join(filepath.Dir(cityDir), "city.toml"), cityDir},
		{"start", cityDir},
	} {
		if out, err := adoptionLatencyPerfCommand(ctx, "", env, 30*time.Second, gcBinary, command...); err != nil {
			return fmt.Errorf("gc %s: %w: %s", command[0], err, strings.TrimSpace(out))
		}
	}
	client, err := genclient.NewClientWithResponses("http://127.0.0.1:" + port)
	if err != nil {
		return fmt.Errorf("create supervisor client: %w", err)
	}
	h := &adoptionLatencyPerfHarness{env: env, gcHome: gcHome, cityDir: cityDir, pid: pid, client: client}
	if err := adoptionLatencyPerfWaitInitialReady(ctx, h); err != nil {
		return fmt.Errorf("initial readiness: %w", err)
	}
	provenance, err := adoptionLatencyPerfProvenance(ctx, h)
	if err != nil {
		return err
	}
	*p, run.FatalError = provenance, ""
	run.Warmup = adoptionLatencySample{Outcome: adoptionLatencyOutcomeError, Error: "warmup interrupted"}
	if !adoptionLatencyPerfCycle(t, ctx, h, &run.Warmup) || run.Warmup.Outcome != adoptionLatencyOutcomeReady {
		for i := range run.Samples {
			run.Samples[i].Error = "warmup failed: " + run.Warmup.Error
		}
		return fmt.Errorf("warmup outcome=%s: %s", run.Warmup.Outcome, run.Warmup.Error)
	}
	failed := false
	for i := range run.Samples {
		if err := ctx.Err(); err != nil {
			cause := fmt.Sprintf("run budget exhausted before sample %d: %v", i+1, err)
			for j := i; j < len(run.Samples); j++ {
				run.Samples[j].Error = cause
			}
			return fmt.Errorf("%s", cause)
		}
		safe := adoptionLatencyPerfCycle(t, ctx, h, &run.Samples[i])
		if run.Samples[i].Outcome != adoptionLatencyOutcomeReady {
			failed = true
			cause := fmt.Sprintf("sample %d outcome=%s: %s", i+1, run.Samples[i].Outcome, run.Samples[i].Error)
			if !safe {
				for j := i + 1; j < len(run.Samples); j++ {
					run.Samples[j].Error = "unsafe to continue after " + cause
				}
				return fmt.Errorf("%s", cause)
			}
		}
	}
	if failed {
		return fmt.Errorf("one or more adoption latency samples failed; see report")
	}
	return nil
}

func adoptionLatencyPerfPrepareCity(t *testing.T, env []string, gcHome string) (string, string, error) {
	root := t.TempDir()
	cityDir, configPath := filepath.Join(root, "c"), filepath.Join(root, "city.toml")
	profile := adoptionLatencyBenchmarkProfileV1()
	config := fmt.Sprintf("[workspace]\nname=%q\n[beads]\nprovider=%q\n[daemon]\nnudge_shadow=%q\nnudge_dispatcher=%q\nsession_reconciler=%q\n[[agent]]\nname=\"worker\"\nstart_command=\"sleep 3600\"\n[[named_session]]\ntemplate=\"worker\"\nmode=\"always\"\n",
		"adopt-"+strconv.FormatInt(time.Now().UnixNano(), 36),
		profile.BeadsProvider,
		profile.ShadowMode,
		profile.DispatcherMode,
		profile.ReconcilerMode,
	)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		return "", "", fmt.Errorf("write city config: %w", err)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		_, _ = runCommand("", env, 10*time.Second, "tmux", "-L", filepath.Base(cityDir), "kill-server")
		unregisterCityCommandEnv(cityDir)
	})
	data, err := os.ReadFile(filepath.Join(gcHome, "supervisor.toml"))
	var port int
	if err != nil {
		return "", "", fmt.Errorf("read supervisor config: %w", err)
	}
	if _, err := fmt.Sscanf(string(data), "[supervisor]\nport = %d", &port); err != nil || port <= 0 {
		return "", "", fmt.Errorf("read supervisor port: %v", err)
	}
	return cityDir, strconv.Itoa(port), nil
}

func adoptionLatencyPerfCycle(t *testing.T, parent context.Context, h *adoptionLatencyPerfHarness, sample *adoptionLatencySample) bool {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	index := sample.Index
	*sample = adoptionLatencySample{Index: index, Outcome: adoptionLatencyOutcomeError, Error: fmt.Sprintf("cycle interrupted while operating on tracked test-owned supervisor PID %d", h.pid)}
	t0 := time.Now()
	if err := adoptionLatencyPerfSignalSupervisor(h.pid); err != nil {
		sample.Error = fmt.Sprintf("signal tracked test-owned supervisor PID %d: %v", h.pid, err)
		return false
	}
	sessionReconcilerColdDisableWaitPIDGone(t, h.pid)
	h.pid = startIsolatedSupervisor(t, h.env, h.gcHome)
	if err := adoptionLatencyPerfWaitReady(ctx, h); err != nil {
		sample.Outcome, sample.Error = adoptionLatencyOutcomeIncomplete, err.Error()
		return false
	}
	t1 := time.Now()
	log, err := os.ReadFile(filepath.Join(h.gcHome, "supervisor.log"))
	if err != nil || len(log) > 1<<20 {
		sample.Error = fmt.Sprintf("read successor log: size=%d err=%v", len(log), err)
		return true
	}
	phases, store, err := parseAdoptionStartupPhases(log)
	if err != nil {
		sample.Error = "parse successor log: " + err.Error()
		return true
	}
	*sample = adoptionLatencySample{Index: index, Outcome: adoptionLatencyOutcomeReady, Duration: t1.Sub(t0), Phases: phases, StartingBeadStore: store}
	return true
}

func adoptionLatencyPerfCommand(ctx context.Context, dir string, env []string, limit time.Duration, binary string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, max(time.Until(deadline), time.Nanosecond))
	}
	return runCommand(dir, env, limit, binary, args...)
}

func adoptionLatencyPerfWaitInitialReady(parent context.Context, h *adoptionLatencyPerfHarness) error {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	return adoptionLatencyPerfWaitReady(ctx, h)
}

func adoptionLatencyPerfWaitReady(ctx context.Context, h *adoptionLatencyPerfHarness) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	capture := h.session.ID == ""
	var last error
	for {
		last = adoptionLatencyPerfAPIRunning(ctx, h)
		got := adoptionLatencyPerfSession{}
		if last == nil {
			got, last = adoptionLatencyPerfSessionState(ctx, h)
		}
		if last == nil && !capture && got != h.session {
			last = fmt.Errorf("durable session changed: got %+v want %+v", got, h.session)
		}
		if last == nil {
			identity, err := adoptionLatencyPerfTmuxIdentity(ctx, h.cityDir, h.env, got.SessionName)
			last = err
			if err == nil {
				if capture {
					h.session, h.tmux = got, identity
				} else if identity != h.tmux {
					last = fmt.Errorf("tmux identity changed: got %q want %q", identity, h.tmux)
				}
			}
		}
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("readiness did not converge: %w (last=%v)", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func adoptionLatencyPerfAPIRunning(ctx context.Context, h *adoptionLatencyPerfHarness) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	response, err := h.client.GetV0CitiesWithResponse(ctx)
	if err != nil {
		return err
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Items == nil {
		return fmt.Errorf("list supervisor cities: status %d: %s", response.StatusCode(), strings.TrimSpace(string(response.Body)))
	}
	for _, city := range *response.JSON200.Items {
		if city.Path == h.cityDir && city.Running {
			if city.Name == "" || h.cityName != "" && city.Name != h.cityName {
				return fmt.Errorf("API reported invalid city identity %q, want %q", city.Name, h.cityName)
			}
			h.cityName = city.Name
			return nil
		}
	}
	return fmt.Errorf("API did not report city running")
}

func adoptionLatencyPerfSessionState(ctx context.Context, h *adoptionLatencyPerfHarness) (adoptionLatencyPerfSession, error) {
	response, err := h.client.GetV0CityByCityNameSessionsWithResponse(ctx, h.cityName, nil)
	if err != nil {
		return adoptionLatencyPerfSession{}, fmt.Errorf("list durable sessions: %w", err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Items == nil || (response.JSON200.Partial != nil && *response.JSON200.Partial) {
		return adoptionLatencyPerfSession{}, fmt.Errorf("list durable sessions: status %d: %s", response.StatusCode(), strings.TrimSpace(string(response.Body)))
	}
	sessions := *response.JSON200.Items
	if len(sessions) != 1 {
		return adoptionLatencyPerfSession{}, fmt.Errorf("durable sessions = %d, want exactly 1 worker", len(sessions))
	}
	session := sessions[0]
	got := adoptionLatencyPerfSession{ID: session.Id, Template: session.Template, SessionName: session.SessionName, State: session.State, Running: session.Running}
	if !got.ready() || got.Template != "worker" || got.ID == "" || got.SessionName == "" {
		return adoptionLatencyPerfSession{}, fmt.Errorf("durable worker session is not ready: %+v", got)
	}
	return got, nil
}

func adoptionLatencyPerfTmuxIdentity(ctx context.Context, cityDir string, env []string, sessionName string) (string, error) {
	out, err := adoptionLatencyPerfCommand(ctx, "", env, 5*time.Second, "tmux", "-L", filepath.Base(cityDir), "display-message", "-p", "-t", "="+sessionName+":", "#{socket_path}|#{session_id}|#{session_name}|#{window_id}|#{pane_id}|#{pane_pid}")
	identity, parts := strings.TrimSpace(out), strings.Split(strings.TrimSpace(out), "|")
	if err != nil || len(parts) != 6 || slices.Contains(parts, "") {
		return "", fmt.Errorf("read complete named tmux identity: %v: %q", err, identity)
	}
	if pid, err := strconv.Atoi(parts[5]); err != nil || pid <= 0 {
		return "", fmt.Errorf("invalid tmux pane PID in %q", identity)
	}
	return identity, nil
}

func adoptionLatencyPerfProvenance(ctx context.Context, h *adoptionLatencyPerfHarness) (adoptionLatencyProvenance, error) {
	payloadPath, ok, err := binaryOverride(integrationDoltBinaryEnv)
	if !ok && err == nil {
		payloadPath, err = exec.LookPath("dolt")
	}
	if err != nil {
		return adoptionLatencyProvenance{}, err
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return adoptionLatencyProvenance{}, err
	}
	specs := []struct {
		name, path string
		args       []string
	}{
		{"gc", gcBinary, []string{"version", "--json"}},
		{"bd_shim", bdBinary, []string{"--version"}},
		{"bd_payload", realBDBinary, []string{"--version"}},
		{"dolt_wrapper", doltBinary, []string{"version"}},
		{"dolt_payload", payloadPath, []string{"version"}},
		{"tmux", tmuxPath, []string{"-V"}},
	}
	binaries := make([]adoptionLatencyBinary, len(specs))
	for i, spec := range specs {
		binaries[i], err = adoptionLatencyPerfBinary(ctx, h, spec.name, spec.path, spec.args...)
		if err != nil {
			return adoptionLatencyProvenance{}, err
		}
	}
	var gc struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(binaries[0].Version))), &gc); err != nil || gc.Version == "" || gc.Commit == "" {
		return adoptionLatencyProvenance{}, fmt.Errorf("decode gc version: %v", err)
	}
	binaries[0].Version = gc.Version
	return adoptionLatencyProvenance{BenchmarkProfile: adoptionLatencyBenchmarkProfileV1(), Binaries: binaries, GCCommit: gc.Commit, RuntimeProvider: parseEnvList(h.env)["GC_SESSION"], RuntimeIdentity: h.tmux, HostOS: runtime.GOOS, HostArch: runtime.GOARCH, CPUCount: runtime.NumCPU()}, nil
}

func adoptionLatencyPerfBinary(ctx context.Context, h *adoptionLatencyPerfHarness, name, path string, args ...string) (adoptionLatencyBinary, error) {
	version, err := adoptionLatencyPerfCommand(ctx, "", h.env, 5*time.Second, path, args...)
	if err != nil {
		return adoptionLatencyBinary{}, fmt.Errorf("%s version: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return adoptionLatencyBinary{}, fmt.Errorf("hash %s: %w", path, err)
	}
	return adoptionLatencyBinary{Name: name, Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Version: strings.TrimSpace(version)}, nil
}

func adoptionLatencyProvenanceForTest() adoptionLatencyProvenance {
	binaries := make([]adoptionLatencyBinary, len(adoptionLatencyRequiredBinaries))
	for i, name := range adoptionLatencyRequiredBinaries {
		binaries[i] = adoptionLatencyBinary{Name: name, Path: "/bin", SHA256: "hash", Version: "version"}
	}
	return adoptionLatencyProvenance{BenchmarkProfile: adoptionLatencyBenchmarkProfileV1(), Binaries: binaries, GCCommit: "commit", RuntimeProvider: "tmux", RuntimeIdentity: "identity", HostOS: "linux", HostArch: "amd64", CPUCount: 1}
}
