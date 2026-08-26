package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func staticFreshManagedDoltDesired(state contract.ConfigState, doltDatabase string) freshManagedDoltDesired {
	return func() (contract.ConfigState, string, error) {
		return state, doltDatabase, nil
	}
}

func writeFreshAdmissionTestCityConfig(t *testing.T, scope, prefix, provider string) {
	t.Helper()
	body := fmt.Sprintf("[workspace]\nname = \"demo\"\nprefix = %q\n\n[beads]\nprovider = %q\n", prefix, provider)
	if err := os.WriteFile(filepath.Join(scope, "city.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFreshManagedDoltWitnessAdmissionBindsCanonicalFiles(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq")
	if err != nil {
		t.Fatalf("prepare admission: %v", err)
	}
	if !needed {
		t.Fatal("fresh scope did not request admission")
	}
	beadsDir := filepath.Join(scope, ".beads")
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err != nil {
		t.Fatalf("bind admission: %v", err)
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, filepath.Join(beadsDir, "dolt"), bdBin, false); err != nil {
		t.Fatalf("validate sealed admission: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte("{\"backend\":\"dolt\",\"project_id\":\"existing\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, filepath.Join(beadsDir, "dolt"), bdBin, false); err == nil {
		t.Fatal("admission accepted metadata changed after sealing")
	}
}

func TestFreshManagedDoltWitnessAdmissionRejectsNonCurrentSelectedBD(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "legacy", body: "printf 'bd version 0.99.0\\n'\n"},
		{name: "unparseable", body: "printf 'bd version unknown\\n'\n"},
		{name: "unrelated current semver", body: "printf 'tool version 9.9.9\\n'\n"},
		{name: "failed command with plausible output", body: "printf 'bd version 1.2.2\\n'\nexit 7\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scope := t.TempDir()
			state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
			if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
				t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
			}
			beadsDir := filepath.Join(scope, ".beads")
			before := snapshotBeadsTreeExact(t, beadsDir)
			bdBin := filepath.Join(t.TempDir(), "bd")
			writeExecutable(t, bdBin, "#!/bin/sh\n"+tt.body)
			if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err == nil {
				t.Fatal("binder sealed admission for non-current selected bd")
			}
			after := snapshotBeadsTreeExact(t, beadsDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused selected bd mutated admission:\nbefore: %#v\nafter:  %#v", before, after)
			}
		})
	}
}

func TestFreshManagedDoltSelectedBDVersionProbeBoundsForkedPipeHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group regression")
	}
	childPIDPath := filepath.Join(t.TempDir(), "child.pid")
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, fmt.Sprintf("#!/bin/sh\nsleep 8 &\nprintf '%%s\\n' \"$!\" > %q\nprintf 'bd version 1.2.2\\n'\n", childPIDPath))
	started := time.Now()
	_, _, err := selectedCurrentBDBinaryIdentity(bdBin)
	elapsed := time.Since(started)
	if data, readErr := os.ReadFile(childPIDPath); readErr == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
			if process, findErr := os.FindProcess(pid); findErr == nil {
				_ = process.Kill()
			}
		}
	}
	if err == nil {
		t.Fatal("forked stdout holder unexpectedly passed the bounded version probe")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("forked stdout holder blocked version probe for %s, want <= 4s", elapsed)
	}
}

func TestAgentImageBDRuntimeVersionIsValidCurrentEraWitness(t *testing.T) {
	t.Parallel()
	dockerfile, err := os.ReadFile(filepath.Join(repoRootForLint(t), "contrib", "k8s", "Dockerfile.agent"))
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "ARG BD_RUNTIME_VERSION="
	var version string
	for _, line := range strings.Split(string(dockerfile), "\n") {
		if strings.HasPrefix(line, prefix) {
			version = strings.TrimPrefix(line, prefix)
			break
		}
	}
	if !currentBeadsVersion(version) {
		t.Fatalf("agent image bd runtime version %q cannot serve as a current-era storage witness", version)
	}
	if !strings.Contains(string(dockerfile), "-X main.Version=${BD_RUNTIME_VERSION}") {
		t.Fatal("agent image does not stamp the validated numeric runtime version into bd")
	}
}

func TestFreshManagedDoltWitnessAdmissionRejectsDesiredIdentityDrift(t *testing.T) {
	tests := []struct {
		name     string
		second   contract.ConfigState
		secondDB string
	}{
		{
			name:     "issue prefix changed",
			second:   contract.ConfigState{IssuePrefix: "new", EndpointOrigin: contract.EndpointOriginManagedCity},
			secondDB: "old_db",
		},
		{
			name:     "database changed",
			second:   contract.ConfigState{IssuePrefix: "old", EndpointOrigin: contract.EndpointOriginManagedCity},
			secondDB: "new_db",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scope := t.TempDir()
			first := contract.ConfigState{IssuePrefix: "old", EndpointOrigin: contract.EndpointOriginManagedCity}
			if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, first, "old_db"); err != nil || !needed {
				t.Fatalf("first admission = (%v, %v), want (true, nil)", needed, err)
			}
			beadsDir := filepath.Join(scope, ".beads")
			before := snapshotBeadsTreeExact(t, beadsDir)
			if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, tt.second, tt.secondDB); err == nil || needed {
				t.Fatalf("drifted admission = (%v, %v), want (false, error)", needed, err)
			}
			after := snapshotBeadsTreeExact(t, beadsDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused desired-state drift mutated admission:\nbefore: %#v\nafter:  %#v", before, after)
			}
		})
	}
}

func TestFreshManagedDoltWitnessAdmissionConflictingCreatorsPublishOneIdentity(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	type admissionCandidate struct {
		prefix   string
		database string
	}
	candidates := []admissionCandidate{{prefix: "old", database: "old_db"}, {prefix: "new", database: "new_db"}}
	start := make(chan struct{})
	results := make(chan struct {
		candidate admissionCandidate
		needed    bool
		err       error
	}, len(candidates))
	for _, candidate := range candidates {
		candidate := candidate
		go func() {
			<-start
			needed, err := prepareFreshManagedDoltWitnessAdmission(scope, contract.ConfigState{
				IssuePrefix:    candidate.prefix,
				EndpointOrigin: contract.EndpointOriginManagedCity,
			}, candidate.database)
			results <- struct {
				candidate admissionCandidate
				needed    bool
				err       error
			}{candidate: candidate, needed: needed, err: err}
		}()
	}
	close(start)
	var winner *admissionCandidate
	for range candidates {
		result := <-results
		if result.err == nil && result.needed {
			if winner != nil {
				t.Fatalf("multiple conflicting admissions succeeded: first=%+v second=%+v", *winner, result.candidate)
			}
			won := result.candidate
			winner = &won
			continue
		}
		if result.err == nil || result.needed {
			t.Fatalf("losing conflicting admission = (%v, %v), want (false, error)", result.needed, result.err)
		}
	}
	if winner == nil {
		t.Fatal("neither conflicting admission published")
	}
	configState, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(scope, ".beads", "config.yaml"))
	if err != nil || !ok {
		t.Fatalf("read winning config = (%+v, %v, %v)", configState, ok, err)
	}
	database, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(scope, ".beads", "metadata.json"))
	if err != nil || !ok {
		t.Fatalf("read winning metadata database = (%q, %v, %v)", database, ok, err)
	}
	if configState.IssuePrefix != winner.prefix || database != winner.database {
		t.Fatalf("published identity = (%q, %q), successful creator = (%q, %q)", configState.IssuePrefix, database, winner.prefix, winner.database)
	}
}

func TestProviderLifecycleBinderBindsFreshAdmissionOnlyForManagedLocalProjection(t *testing.T) {
	tests := []struct {
		name       string
		transition func(*testing.T, string)
		wantState  string
	}{
		{
			name:      "managed local",
			wantState: freshManagedDoltAdmissionSealed,
		},
		{
			name: "external endpoint",
			transition: func(t *testing.T, scope string) {
				t.Helper()
				if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, scope, contract.ConfigState{
					IssuePrefix:    "gc",
					EndpointOrigin: contract.EndpointOriginCityCanonical,
					EndpointStatus: contract.EndpointStatusVerified,
					DoltHost:       "db.example.com",
					DoltPort:       "3306",
				}); err != nil {
					t.Fatalf("switch admission fixture to external endpoint: %v", err)
				}
			},
			wantState: freshManagedDoltAdmissionAwaitingBD,
		},
		{
			name: "doltlite backend",
			transition: func(t *testing.T, _ string) {
				t.Helper()
				t.Setenv("GC_BEADS_BACKEND", "doltlite")
				t.Setenv("BEADS_BACKEND", "doltlite")
			},
			wantState: freshManagedDoltAdmissionAwaitingBD,
		},
		{
			name: "hosted credential bridge",
			transition: func(t *testing.T, _ string) {
				t.Helper()
				t.Setenv("GC_DOLT_CRED_CMD", "/bin/false")
			},
			wantState: freshManagedDoltAdmissionAwaitingBD,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := t.TempDir()
			materializeBuiltinPacksForTest(t, scope)
			provider := "exec:" + gcBeadsBdScriptPath(scope)
			t.Setenv("GC_BEADS", provider)
			t.Setenv("GC_BEADS_SCOPE_ROOT", scope)
			writeFreshAdmissionTestCityConfig(t, scope, "gc", provider)

			state := desiredCityDoltConfigState(scope, config.DoltConfig{}, "gc")
			if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
				t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
			}
			bdBin := filepath.Join(t.TempDir(), "bd")
			writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
			t.Setenv("BD_BIN", bdBin)
			if tt.transition != nil {
				tt.transition(t, scope)
			}

			env, err := providerLifecycleProcessEnvWithError(scope, provider)
			if err != nil {
				t.Fatalf("providerLifecycleProcessEnvWithError: %v", err)
			}
			beforeBind, err := readFreshManagedDoltAdmission(filepath.Join(scope, ".beads", freshManagedDoltAdmissionName))
			if err != nil {
				t.Fatal(err)
			}
			if beforeBind.State != freshManagedDoltAdmissionAwaitingBD {
				t.Fatalf("environment construction mutated admission state to %q", beforeBind.State)
			}
			if tt.name == "hosted credential bridge" && strings.TrimSpace(runtimeEnvEntriesToMap(env)["BEADS_DOLT_CREDENTIAL_COMMAND"]) == "" {
				t.Fatal("precondition failed: hosted credential bridge was not projected")
			}
			if err := bindFreshManagedDoltAdmissionForProviderEnv(scope, env); err != nil {
				t.Fatalf("bind at provider operation boundary: %v", err)
			}
			record, err := readFreshManagedDoltAdmission(filepath.Join(scope, ".beads", freshManagedDoltAdmissionName))
			if err != nil {
				t.Fatal(err)
			}
			if record.State != tt.wantState {
				t.Fatalf("admission state = %q, want %q", record.State, tt.wantState)
			}
		})
	}
}

func TestProviderLifecycleBinderGateLeavesShellRemoteAliasesAwaiting(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"127.0.0.2", "LOCALHOST", "::", "[::]"} {
		host := host
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			scope := t.TempDir()
			state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
			if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
				t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
			}
			bdBin := filepath.Join(t.TempDir(), "bd")
			writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
			if err := bindFreshManagedDoltAdmissionForProviderEnv(scope, []string{
				"BD_BIN=" + bdBin,
				"GC_DOLT_HOST=" + host,
			}); err != nil {
				t.Fatal(err)
			}
			record, err := readFreshManagedDoltAdmission(filepath.Join(scope, ".beads", freshManagedDoltAdmissionName))
			if err != nil {
				t.Fatal(err)
			}
			if record.State != freshManagedDoltAdmissionAwaitingBD {
				t.Fatalf("admission state = %q, want %q", record.State, freshManagedDoltAdmissionAwaitingBD)
			}
		})
	}
}

func TestProviderLifecycleBinderGateMirrorsWhitespaceCredentialBypass(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	if err := bindFreshManagedDoltAdmissionForProviderEnv(scope, []string{
		"BD_BIN=" + bdBin,
		"GC_DOLT_HOST=127.0.0.1",
		"BEADS_DOLT_CREDENTIAL_COMMAND= ",
	}); err != nil {
		t.Fatal(err)
	}
	record, err := readFreshManagedDoltAdmission(filepath.Join(scope, ".beads", freshManagedDoltAdmissionName))
	if err != nil {
		t.Fatal(err)
	}
	if record.State != freshManagedDoltAdmissionAwaitingBD {
		t.Fatalf("admission state = %q, want %q", record.State, freshManagedDoltAdmissionAwaitingBD)
	}
}

func TestProviderLifecycleManagedLocalHostExactlyMirrorsProviderShell(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		host string
		want bool
	}{
		{host: "", want: true},
		{host: "127.0.0.1", want: true},
		{host: "0.0.0.0", want: true},
		{host: "localhost", want: true},
		{host: "::1", want: true},
		{host: "[::1]", want: true},
		{host: "127.0.0.2"},
		{host: "LOCALHOST"},
		{host: "::"},
		{host: "[::]"},
		{host: "db.example.com"},
	} {
		t.Run(tt.host, func(t *testing.T) {
			got := providerLifecycleEnvUsesManagedLocalDolt([]string{"GC_DOLT_HOST=" + tt.host})
			if got != tt.want {
				t.Fatalf("providerLifecycleEnvUsesManagedLocalDolt(host=%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestStartBeadsLifecycleRefusesFreshAdmissionPreparedForPriorConfig(t *testing.T) {
	scope := t.TempDir()
	materializeBuiltinPacksForTest(t, scope)
	script := gcBeadsBdScriptPath(scope)
	provider := "exec:" + script
	providerLog := filepath.Join(t.TempDir(), "provider.log")
	writeExecutable(t, script, fmt.Sprintf("#!/bin/sh\nprintf 'invoked\\n' >> %q\nexit 97\n", providerLog))
	writeFreshAdmissionTestCityConfig(t, scope, "old", provider)
	t.Setenv("GC_BEADS", provider)
	t.Setenv("GC_BEADS_SCOPE_ROOT", scope)

	bdLog := filepath.Join(t.TempDir(), "bd.log")
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, fmt.Sprintf("#!/bin/sh\nprintf 'invoked\\n' >> %q\nprintf 'bd version 1.2.2\\n'\n", bdLog))
	t.Setenv("BD_BIN", bdBin)

	if err := seedDeferredManagedBeadsErr(scope, scope, "old", "hq"); err != nil {
		t.Fatalf("seed old desired admission: %v", err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	before := snapshotBeadsTreeExact(t, beadsDir)

	writeFreshAdmissionTestCityConfig(t, scope, "new", provider)
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(scope, io.Discard)
	if err != nil {
		t.Fatalf("load changed city config: %v", err)
	}
	err = startBeadsLifecycle(scope, "demo", cfg, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "current desired") {
		t.Fatalf("startBeadsLifecycle error = %v, want stale desired-admission refusal", err)
	}
	if _, err := os.Stat(providerLog); !os.IsNotExist(err) {
		t.Fatalf("provider ran despite stale desired admission, stat err = %v", err)
	}
	if _, err := os.Stat(bdLog); !os.IsNotExist(err) {
		t.Fatalf("selected BD was probed despite stale desired admission, stat err = %v", err)
	}
	after := snapshotBeadsTreeExact(t, beadsDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("stale desired refusal mutated .beads:\nbefore: %#v\nafter:  %#v", before, after)
	}
	for _, unexpected := range []string{".local_version", "dolt"} {
		if _, err := os.Lstat(filepath.Join(beadsDir, unexpected)); !os.IsNotExist(err) {
			t.Fatalf("stale desired refusal created %s, stat err = %v", unexpected, err)
		}
	}
}

func TestEnsureBeadsProviderBindsFreshAdmissionForCanonicalCustomWrapper(t *testing.T) {
	scope := t.TempDir()
	materializeBuiltinPacksForTest(t, scope)
	callLog := filepath.Join(t.TempDir(), "provider.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd.sh")
	writeExecutable(t, script, fmt.Sprintf(
		"#!/bin/sh\ngrep -q '\"state\":\"sealed\"' \"$GC_CITY_PATH/.beads/%s\" || exit 91\nprintf '%%s\\n' \"$1\" > %q\nexit 17\n",
		freshManagedDoltAdmissionName,
		callLog,
	))
	provider := "exec:" + script
	writeFreshAdmissionTestCityConfig(t, scope, "gc", provider)
	t.Setenv("GC_BEADS", provider)
	t.Setenv("GC_BEADS_SCOPE_ROOT", scope)

	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	t.Setenv("BD_BIN", bdBin)
	state := desiredCityDoltConfigState(scope, config.DoltConfig{}, "gc")
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}

	if err := ensureBeadsProvider(scope); err == nil || !strings.Contains(err.Error(), "exit status 17") {
		t.Fatalf("ensure custom canonical provider error = %v, want post-bind stub exit 17", err)
	}
	if got, err := os.ReadFile(callLog); err != nil {
		t.Fatalf("read custom provider call log: %v", err)
	} else if strings.TrimSpace(string(got)) != "start" {
		t.Fatalf("custom provider operation = %q, want start", got)
	}
	record, err := readFreshManagedDoltAdmission(filepath.Join(scope, ".beads", freshManagedDoltAdmissionName))
	if err != nil {
		t.Fatal(err)
	}
	if record.State != freshManagedDoltAdmissionSealed || record.BDBinary != bdBin {
		t.Fatalf("custom-wrapper admission = %+v, want sealed exact BD %q", record, bdBin)
	}
}

func TestShutdownBeadsProviderSkipsRootlessFreshAdmission(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		name := "awaiting"
		if sealed {
			name = "sealed"
		}
		t.Run(name, func(t *testing.T) {
			scope := t.TempDir()
			materializeBuiltinPacksForTest(t, scope)
			callLog := filepath.Join(t.TempDir(), "provider.log")
			script := filepath.Join(t.TempDir(), "gc-beads-bd.sh")
			writeExecutable(t, script, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$1\" > %q\nexit 91\n", callLog))
			provider := "exec:" + script
			writeFreshAdmissionTestCityConfig(t, scope, "gc", provider)
			t.Setenv("GC_BEADS", provider)
			t.Setenv("GC_BEADS_SCOPE_ROOT", scope)

			state := desiredCityDoltConfigState(scope, config.DoltConfig{}, "gc")
			if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
				t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
			}
			if sealed {
				bdBin := filepath.Join(t.TempDir(), "bd")
				writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
				if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err != nil {
					t.Fatalf("seal admission fixture: %v", err)
				}
			}
			beadsDir := filepath.Join(scope, ".beads")
			before := snapshotBeadsTreeExact(t, beadsDir)

			if err := shutdownBeadsProvider(scope); err != nil {
				t.Fatalf("shutdownBeadsProvider: %v", err)
			}
			if _, err := os.Stat(callLog); !os.IsNotExist(err) {
				t.Fatalf("provider stop ran for rootless %s admission, stat err = %v", name, err)
			}
			after := snapshotBeadsTreeExact(t, beadsDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("shutdown mutated rootless %s admission:\nbefore: %#v\nafter:  %#v", name, before, after)
			}
			for _, unexpected := range []string{".local_version", "dolt"} {
				if _, err := os.Lstat(filepath.Join(beadsDir, unexpected)); !os.IsNotExist(err) {
					t.Fatalf("shutdown created %s for rootless %s admission, stat err = %v", unexpected, name, err)
				}
			}
		})
	}
}

func TestFreshManagedDoltBinderRefusesDesiredDatabaseDriftBeforeBDProbe(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "old_db"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	before := snapshotBeadsTreeExact(t, beadsDir)
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, fmt.Sprintf("#!/bin/sh\nprintf 'invoked\\n' >> %q\nprintf 'bd version 1.2.2\\n'\n", bdLog))

	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "new_db")); err == nil {
		t.Fatal("binder sealed admission after desired database changed")
	}
	if _, err := os.Stat(bdLog); !os.IsNotExist(err) {
		t.Fatalf("selected BD was probed despite desired database drift, stat err = %v", err)
	}
	after := snapshotBeadsTreeExact(t, beadsDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("database-drift refusal mutated .beads:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestFreshManagedDoltBinderRevalidatesDesiredWhenAdmissionDisappeared(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	calls := 0
	err := bindFreshManagedDoltAdmissionToBD(scope, filepath.Join(t.TempDir(), "missing-bd"), func() (contract.ConfigState, string, error) {
		calls++
		return contract.ConfigState{}, "", fmt.Errorf("endpoint changed")
	})
	if err == nil || !strings.Contains(err.Error(), "endpoint changed") {
		t.Fatalf("binder error = %v, want desired-state revalidation failure", err)
	}
	if calls != 1 {
		t.Fatalf("desired callback calls = %d, want 1 at absent-admission boundary", calls)
	}
}

func TestProviderLifecycleBinderRefusesRetainedLocalEnvAfterExternalTransition(t *testing.T) {
	scope := t.TempDir()
	materializeBuiltinPacksForTest(t, scope)
	provider := "exec:" + gcBeadsBdScriptPath(scope)
	body := fmt.Sprintf("[workspace]\nname = \"demo\"\nprefix = \"gc\"\n\n[beads]\nprovider = %q\n\n[dolt]\nhost = \"db.example.com\"\nport = 4406\n", provider)
	if err := os.WriteFile(filepath.Join(scope, "city.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, fmt.Sprintf("#!/bin/sh\nprintf 'invoked\\n' > %q\nprintf 'bd version 1.2.2\\n'\n", bdLog))

	err := bindFreshManagedDoltAdmissionForProviderEnv(scope, []string{"BD_BIN=" + bdBin})
	if err == nil || !strings.Contains(err.Error(), "desired endpoint changed") {
		t.Fatalf("binder error = %v, want stale managed-local environment refusal", err)
	}
	if _, err := os.Stat(bdLog); !os.IsNotExist(err) {
		t.Fatalf("stale local environment probed BD after external transition, stat err = %v", err)
	}
}

func TestFreshManagedDoltBinderWinsEndpointTransitionRace(t *testing.T) {
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	admissionPath := filepath.Join(beadsDir, freshManagedDoltAdmissionName)
	if info, err := os.Lstat(admissionPath); err != nil {
		t.Fatal(err)
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("awaiting admission mode = %v, want regular 0600", info.Mode())
	}

	probeDir := t.TempDir()
	probeStarted := filepath.Join(probeDir, "started")
	releaseProbe := filepath.Join(probeDir, "release")
	bdBin := filepath.Join(probeDir, "bd")
	writeExecutable(t, bdBin, fmt.Sprintf(
		"#!/bin/sh\n: > %q\nwhile [ ! -e %q ]; do sleep 0.01; done\nprintf 'bd version 1.2.2\\n'\n",
		probeStarted,
		releaseProbe,
	))
	binderResult := make(chan error, 1)
	go func() {
		binderResult <- bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq"))
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(probeStarted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("selected-BD probe did not reach its controlled lock hold")
		}
		time.Sleep(10 * time.Millisecond)
	}

	type endpointResult struct {
		guard *freshManagedDoltExternalTransitionGuard
		err   error
	}
	endpointResults := make(chan endpointResult, 1)
	go func() {
		guard, err := lockAwaitingFreshManagedDoltAdmissionForExternalTransition(scope, state, "hq")
		endpointResults <- endpointResult{guard: guard, err: err}
	}()
	select {
	case result := <-endpointResults:
		if result.guard != nil {
			result.guard.release()
		}
		t.Fatalf("endpoint contender returned before the binding lock was released: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := os.WriteFile(releaseProbe, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-binderResult:
		if err != nil {
			t.Fatalf("binding winner: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("binding winner did not finish")
	}
	select {
	case result := <-endpointResults:
		if result.guard != nil {
			result.guard.release()
			t.Fatal("endpoint transition acquired a guard after the binder sealed admission")
		}
		if result.err == nil || !strings.Contains(result.err.Error(), "sealed") {
			t.Fatalf("endpoint transition error = %v, want sealed-admission refusal", result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("endpoint contender did not observe the sealed admission")
	}

	info, err := os.Lstat(admissionPath)
	if err != nil {
		t.Fatalf("sealed admission path disappeared: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("sealed admission mode = %v, want regular 0600", info.Mode())
	}
	record, err := readFreshManagedDoltAdmission(admissionPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != freshManagedDoltAdmissionSealed || record.BDBinary != bdBin {
		t.Fatalf("binding winner record = %+v, want sealed exact BD %q", record, bdBin)
	}
	for _, unexpected := range []string{".local_version", "dolt"} {
		if _, err := os.Lstat(filepath.Join(beadsDir, unexpected)); !os.IsNotExist(err) {
			t.Fatalf("binding race created %s before provider invocation, stat err = %v", unexpected, err)
		}
	}
}

func TestFreshManagedDoltEndpointTransitionWinsBinderRace(t *testing.T) {
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	admissionPath := filepath.Join(beadsDir, freshManagedDoltAdmissionName)
	guard, err := lockAwaitingFreshManagedDoltAdmissionForExternalTransition(scope, state, "hq")
	if err != nil {
		t.Fatalf("lock endpoint transition: %v", err)
	}
	if guard == nil {
		t.Fatal("endpoint transition did not acquire the awaiting admission")
	}
	defer guard.release()
	if info, err := os.Lstat(admissionPath); err != nil {
		t.Fatal(err)
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("locked admission mode = %v, want regular 0600", info.Mode())
	}

	bdLog := filepath.Join(t.TempDir(), "bd.log")
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, fmt.Sprintf("#!/bin/sh\nprintf 'invoked\\n' > %q\nprintf 'bd version 1.2.2\\n'\n", bdLog))
	endpointChanged := make(chan struct{})
	desired := func() (contract.ConfigState, string, error) {
		select {
		case <-endpointChanged:
			return contract.ConfigState{}, "", fmt.Errorf("endpoint changed")
		default:
			return state, "hq", nil
		}
	}
	binderStarted := make(chan struct{})
	binderResult := make(chan error, 1)
	go func() {
		close(binderStarted)
		binderResult <- bindFreshManagedDoltAdmissionToBD(scope, bdBin, desired)
	}()
	<-binderStarted
	select {
	case err := <-binderResult:
		t.Fatalf("binder returned before the endpoint released its admission lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(endpointChanged)
	if err := guard.discard(); err != nil {
		t.Fatalf("discard endpoint-winning admission: %v", err)
	}
	guard.release()
	select {
	case err := <-binderResult:
		if err == nil {
			t.Fatal("binder succeeded after the endpoint transition discarded admission")
		}
		if !strings.Contains(err.Error(), "admission") && !strings.Contains(err.Error(), "endpoint changed") {
			t.Fatalf("binder error = %v, want admission/current-desired refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("losing binder remained blocked after endpoint transition")
	}

	if _, err := os.Lstat(admissionPath); !os.IsNotExist(err) {
		t.Fatalf("endpoint-winning admission path still exists, stat err = %v", err)
	}
	if _, err := os.Stat(bdLog); !os.IsNotExist(err) {
		t.Fatalf("losing binder invoked selected BD, stat err = %v", err)
	}
	for _, unexpected := range []string{".local_version", "dolt"} {
		if _, err := os.Lstat(filepath.Join(beadsDir, unexpected)); !os.IsNotExist(err) {
			t.Fatalf("endpoint-winning race created %s, stat err = %v", unexpected, err)
		}
	}
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".gascity-fresh-admission-") {
			t.Fatalf("endpoint-winning race left admission temp %q", entry.Name())
		}
	}
}

func TestProviderLifecycleBinderRechecksDesiredConfigAfterBDProbe(t *testing.T) {
	scope := t.TempDir()
	materializeBuiltinPacksForTest(t, scope)
	provider := "exec:" + gcBeadsBdScriptPath(scope)
	writeFreshAdmissionTestCityConfig(t, scope, "old", provider)
	state := desiredCityDoltConfigState(scope, config.DoltConfig{}, "old")
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	before := snapshotBeadsTreeExact(t, beadsDir)

	probeDir := t.TempDir()
	started := filepath.Join(probeDir, "started")
	release := filepath.Join(probeDir, "release")
	bdBin := filepath.Join(probeDir, "bd")
	writeExecutable(t, bdBin, fmt.Sprintf("#!/bin/sh\n: > %q\nwhile [ ! -e %q ]; do sleep 0.01; done\nprintf 'bd version 1.2.2\\n'\n", started, release))
	result := make(chan error, 1)
	go func() {
		result <- bindFreshManagedDoltAdmissionForProviderEnv(scope, []string{"BD_BIN=" + bdBin})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("selected BD version probe did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeFreshAdmissionTestCityConfig(t, scope, "new", provider)
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "current desired") {
		t.Fatalf("binder error = %v, want post-probe desired-state refusal", err)
	}
	after := snapshotBeadsTreeExact(t, beadsDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("post-probe desired refusal mutated .beads:\nbefore: %#v\nafter:  %#v", before, after)
	}
	record, err := readFreshManagedDoltAdmission(filepath.Join(beadsDir, freshManagedDoltAdmissionName))
	if err != nil {
		t.Fatal(err)
	}
	if record.State != freshManagedDoltAdmissionAwaitingBD || record.BDBinary != "" || record.BDSHA256 != "" {
		t.Fatalf("post-probe refusal sealed stale admission: %#v", record)
	}
}

func TestFreshManagedDoltAdmissionComposesThroughRealValidatorAndProviderFirstStart(t *testing.T) {
	scope := t.TempDir()
	materializeBuiltinPacksForTest(t, scope)
	script := gcBeadsBdScriptPath(scope)
	provider := "exec:" + script
	if err := os.WriteFile(filepath.Join(scope, "city.toml"), []byte("[workspace]\nname = \"demo\"\nprefix = \"gc\"\n\n[beads]\nprovider = \""+provider+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", provider)
	t.Setenv("GC_BEADS_SCOPE_ROOT", scope)

	binDir := t.TempDir()
	bdBin := filepath.Join(binDir, "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\ncase \"${1:-}\" in version) printf 'bd version 1.2.2\\n' ;; *) exit 64 ;; esac\n")
	t.Setenv("BD_BIN", bdBin)

	if err := seedDeferredManagedBeadsErr(scope, scope, "gc", "hq"); err != nil {
		t.Fatalf("seed deferred managed beads: %v", err)
	}
	admissionPath := filepath.Join(scope, ".beads", freshManagedDoltAdmissionName)
	seeded, err := readFreshManagedDoltAdmission(admissionPath)
	if err != nil {
		t.Fatalf("read seeded admission: %v", err)
	}
	if seeded.State != freshManagedDoltAdmissionAwaitingBD {
		t.Fatalf("seeded admission state = %q, want %q", seeded.State, freshManagedDoltAdmissionAwaitingBD)
	}

	reexecGC := reexecGCTestBinaryForTests(t)
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return reexecGC }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })
	providerEnv, err := providerLifecycleProcessEnvWithError(scope, provider)
	if err != nil {
		t.Fatalf("bind provider lifecycle admission: %v", err)
	}
	if err := bindFreshManagedDoltAdmissionForProviderEnv(scope, providerEnv); err != nil {
		t.Fatalf("bind provider lifecycle admission: %v", err)
	}
	bound, err := readFreshManagedDoltAdmission(admissionPath)
	if err != nil {
		t.Fatalf("read bound admission: %v", err)
	}
	if bound.State != freshManagedDoltAdmissionSealed || bound.BDBinary != bdBin {
		t.Fatalf("bound admission = %+v, want sealed exact BD %q", bound, bdBin)
	}

	providerEnv = overlayEnvEntries(providerEnv, map[string]string{
		"GC_DOLT":      "managed",
		"GC_DOLT_PORT": "19999",
	})
	stdout, stderr, runErr := runIdentityTestCommand(script, []string{"future-provider-operation"}, providerEnv)
	if runErr == nil {
		t.Fatalf("unknown provider operation unexpectedly succeeded:\n%s%s", stdout, stderr)
	}
	beadsDir := filepath.Join(scope, ".beads")
	if got, err := os.ReadFile(filepath.Join(beadsDir, ".local_version")); err != nil {
		t.Fatalf("read first-start witness: %v", err)
	} else if strings.TrimSpace(string(got)) != "1.2.2" {
		t.Fatalf("first-start witness = %q, want 1.2.2", got)
	}
	if info, err := os.Lstat(filepath.Join(beadsDir, "dolt")); err != nil {
		t.Fatalf("first-start Dolt root missing: %v", err)
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("first-start Dolt root mode = %v, want real directory", info.Mode())
	}
	if _, err := os.Lstat(admissionPath); !os.IsNotExist(err) {
		t.Fatalf("validated admission was not consumed, stat err = %v", err)
	}
}

func TestFreshManagedDoltWitnessAdmissionResumesOnlyBoundCrashStates(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	dataRoot := filepath.Join(beadsDir, "dolt")
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err != nil {
		t.Fatalf("bind admission: %v", err)
	}

	// Crash before witness creation: the sealed, rootless state remains valid.
	if err := validateFreshManagedDoltWitnessAdmission(scope, dataRoot, bdBin, false); err != nil {
		t.Fatalf("validate pre-witness state: %v", err)
	}
	// Crash after the no-clobber witness link: only a valid current-era marker
	// may resume while the data root is still absent.
	if err := os.WriteFile(filepath.Join(beadsDir, ".local_version"), []byte("1.2.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, dataRoot, bdBin, false); err != nil {
		t.Fatalf("validate post-witness state: %v", err)
	}
	tempLink := filepath.Join(beadsDir, ".local_version.tmp.A1b2C3")
	if err := os.Link(filepath.Join(beadsDir, ".local_version"), tempLink); err != nil {
		t.Fatal(err)
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, dataRoot, bdBin, false); err != nil {
		t.Fatalf("validate post-link/pre-unlink crash state: %v", err)
	}
	if err := os.Remove(tempLink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempLink, []byte("1.2.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, dataRoot, bdBin, false); err == nil {
		t.Fatal("admission accepted a divergent witness temp")
	}
	if err := os.Remove(tempLink); err != nil {
		t.Fatal(err)
	}
	// Crash after root creation but before admission unlink has its own strict
	// validation mode and requires both the bound witness and real directory.
	if err := os.Mkdir(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, dataRoot, bdBin, false); err == nil {
		t.Fatal("ordinary fresh validation accepted an already-created data root")
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, dataRoot, bdBin, true); err != nil {
		t.Fatalf("validate post-root resume state: %v", err)
	}
	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err != nil {
		t.Fatalf("bind post-root resume state: %v", err)
	}
}

func TestFreshManagedDoltWitnessAdmissionNeverSealsAfterRootAppears(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	dataRoot := filepath.Join(beadsDir, "dolt")
	if err := os.WriteFile(filepath.Join(beadsDir, ".local_version"), []byte("1.2.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err == nil {
		t.Fatal("binder sealed an awaiting admission after its data root appeared")
	}
	record, err := readFreshManagedDoltAdmission(filepath.Join(beadsDir, freshManagedDoltAdmissionName))
	if err != nil {
		t.Fatal(err)
	}
	if record.State != freshManagedDoltAdmissionAwaitingBD || record.BDBinary != "" || record.BDSHA256 != "" {
		t.Fatalf("refused post-root admission was mutated: %#v", record)
	}
}

func TestFreshManagedDoltWitnessAdmissionRejectsPopulatedCrashRootWithoutMutation(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	dataRoot := filepath.Join(beadsDir, "dolt")
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, ".local_version"), []byte("1.2.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "legacy-data"), []byte("must survive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotBeadsTreeExact(t, beadsDir)
	if err := validateFreshManagedDoltWitnessAdmission(scope, dataRoot, bdBin, true); err == nil {
		t.Fatal("post-root validator accepted populated storage")
	}
	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err == nil {
		t.Fatal("binder accepted populated post-root storage")
	}
	after := snapshotBeadsTreeExact(t, beadsDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("refused populated root was mutated:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestFreshManagedDoltWitnessAdmissionRejectsWrongScopeAndBD(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if _, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil {
		t.Fatal(err)
	}
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err != nil {
		t.Fatal(err)
	}
	wrongBD := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, wrongBD, "#!/bin/sh\nprintf 'bd version 9.9.9\\n'\n")
	dataRoot := filepath.Join(scope, ".beads", "dolt")
	if err := validateFreshManagedDoltWitnessAdmission(scope, dataRoot, wrongBD, false); err == nil {
		t.Fatal("admission accepted a different bd executable")
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, filepath.Join(t.TempDir(), "dolt"), bdBin, false); err == nil {
		t.Fatal("admission accepted a different data root")
	}
}

func TestFreshManagedDoltWitnessAdmissionAwaitingStateCannotBlessExistingWitness(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.WriteFile(filepath.Join(beadsDir, ".local_version"), []byte("1.2.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotBeadsTreeExact(t, beadsDir)
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	if err := bindFreshManagedDoltAdmissionToBD(scope, bdBin, staticFreshManagedDoltDesired(state, "hq")); err == nil {
		t.Fatal("binder retroactively blessed a witness created before BD binding")
	}
	after := snapshotBeadsTreeExact(t, beadsDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("refused pre-binding witness mutated admission state:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestFreshManagedDoltWitnessAdmissionBindingIsSingleWriter(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
	if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err != nil || !needed {
		t.Fatalf("prepare admission = (%v, %v), want (true, nil)", needed, err)
	}
	bdBins := []string{filepath.Join(t.TempDir(), "bd-a"), filepath.Join(t.TempDir(), "bd-b")}
	writeExecutable(t, bdBins[0], "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	writeExecutable(t, bdBins[1], "#!/bin/sh\nprintf 'bd version 9.9.9\\n'\n")
	start := make(chan struct{})
	results := make(chan struct {
		index int
		err   error
	}, len(bdBins))
	for i, bdBin := range bdBins {
		go func(index int, selected string) {
			<-start
			results <- struct {
				index int
				err   error
			}{index: index, err: bindFreshManagedDoltAdmissionToBD(scope, selected, staticFreshManagedDoltDesired(state, "hq"))}
		}(i, bdBin)
	}
	close(start)
	winner := -1
	for range bdBins {
		result := <-results
		if result.err == nil {
			if winner >= 0 {
				t.Fatalf("multiple BD identities sealed the admission: %d and %d", winner, result.index)
			}
			winner = result.index
		}
	}
	if winner < 0 {
		t.Fatal("neither BD identity sealed the admission")
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, filepath.Join(scope, ".beads", "dolt"), bdBins[winner], false); err != nil {
		t.Fatalf("winning BD identity does not validate: %v", err)
	}
	if err := validateFreshManagedDoltWitnessAdmission(scope, filepath.Join(scope, ".beads", "dolt"), bdBins[1-winner], false); err == nil {
		t.Fatal("losing BD identity validates against the sealed admission")
	}
}

func TestFreshManagedDoltWitnessAdmissionRefusesUnprovenRootlessLayoutsWithoutMutation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		metadata string
	}{
		{name: "corrupt", metadata: "{not-json\n"},
		{name: "unknown", metadata: `{"database":"mystery","backend":"mystery"}` + "\n"},
		{name: "remote", metadata: `{"database":"dolt","backend":"dolt","project_id":"existing","dolt_server_host":"remote"}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := t.TempDir()
			beadsDir := filepath.Join(scope, ".beads")
			if err := os.Mkdir(beadsDir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue-prefix: legacy\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(tc.metadata), 0o640); err != nil {
				t.Fatal(err)
			}
			before := snapshotBeadsTreeExact(t, beadsDir)
			state := contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity}
			if needed, err := prepareFreshManagedDoltWitnessAdmission(scope, state, "hq"); err == nil || needed {
				t.Fatalf("prepare admission = (%v, %v), want fail-closed refusal", needed, err)
			}
			after := snapshotBeadsTreeExact(t, beadsDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused layout mutated:\nbefore: %#v\nafter:  %#v", before, after)
			}
		})
	}
}
