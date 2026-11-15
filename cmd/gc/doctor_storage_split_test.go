package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// stubGCVersion runs the test as a gc of version v.
func stubGCVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

// stubSessionPathGC makes the session PATH's gc report v ("" for no gc).
func stubSessionPathGC(t *testing.T, v string, err error) {
	t.Helper()
	prev := minGCVersionPathProbe
	minGCVersionPathProbe = func(*config.City) (string, string, error) {
		if v == "" && err == nil {
			return "", "", nil
		}
		return "/usr/local/bin/gc", v, err
	}
	t.Cleanup(func() { minGCVersionPathProbe = prev })
}

// convergedSplitCity is a split city whose binding converged.
func convergedSplitCity(t *testing.T) (storageOperatorRequest, infraBindingTarget) {
	t.Helper()
	request, cfg, _ := preflightReadyCity(t, 2)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, request.CityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the fixture did not converge: %s: %s", got.Outcome, log.String())
	}
	return request, mustResolveInfraTarget(t, request.CityPath, cfg)
}

// TestStorageSplitDoctorAgreesWithStatus runs `gc storage status` and the
// storage-split check over the same fixtures: the check errors exactly when
// status's city verdict exits non-zero, and each WARN and ERROR row of the
// design's table is reached.
func TestStorageSplitDoctorAgreesWithStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T) storageOperatorRequest
		status doctor.CheckStatus
		want   string
		detail string
	}{
		{
			name: "configured and not converged",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _, _ := preflightReadyCity(t, 2)
				return request
			},
			status: doctor.StatusError, want: "not serving",
		},
		{
			name: "converged",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _ := convergedSplitCity(t)
				return request
			},
			status: doctor.StatusOK, want: "split, converged, and serving",
		},
		{
			name: "identity mismatch",
			setup: func(t *testing.T) storageOperatorRequest {
				request, target := convergedSplitCity(t)
				data, err := os.ReadFile(target.MarkerPath())
				if err != nil {
					t.Fatal(err)
				}
				var kept []string
				for _, line := range strings.Split(string(data), "\n") {
					if !strings.HasPrefix(line, workStoreIdentityLinePrefix) {
						kept = append(kept, line)
					}
				}
				other := workStoreIdentityLinePrefix + workStoreIdentity{ProjectID: "another-project"}.String()
				if err := os.WriteFile(target.MarkerPath(), []byte(other+"\n"+strings.Join(kept, "\n")), 0o644); err != nil {
					t.Fatal(err)
				}
				return request
			},
			status: doctor.StatusError, want: "not serving",
		},
		{
			// substrate 2026-11-09: a plain `bd create` carrying
			// gc.root_bead_id. The city serves degraded, and doctor says so
			// as an error, because nothing else stops it now.
			name: "converged with a stranded raw write",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _ := convergedSplitCity(t)
				source, err := openInfraMigrationSource(request.CityPath)
				if err != nil {
					t.Fatal(err)
				}
				mustCreateInfraBead(t, source, beads.Bead{Title: "patrol finding", Type: "task", Metadata: map[string]string{"gc.root_bead_id": "gcg-1"}})
				// Tied to the gate: doctor's "degraded" is the gate's verdict.
				routes, err := storageBootGate(request.CityPath, request.Cfg, "gc start", nil, io.Discard)
				if err != nil || !routes.bootVerdict.servesDegraded() {
					t.Fatalf("the gate does not serve this city degraded (err %v)", err)
				}
				_ = routes.close()
				return request
			},
			status: doctor.StatusError, want: "the city serves degraded",
		},
		{
			name: "legacy marker without identity",
			setup: func(t *testing.T) storageOperatorRequest {
				request, target := convergedSplitCity(t)
				data, err := os.ReadFile(target.MarkerPath())
				if err != nil {
					t.Fatal(err)
				}
				var kept []string
				for _, line := range strings.Split(string(data), "\n") {
					if !strings.HasPrefix(line, workStoreIdentityLinePrefix) {
						kept = append(kept, line)
					}
				}
				if err := os.WriteFile(target.MarkerPath(), []byte(strings.Join(kept, "\n")), 0o644); err != nil {
					t.Fatal(err)
				}
				return request
			},
			status: doctor.StatusWarning, want: "records no work-store identity",
		},
		{
			name: "legacy satellites",
			setup: func(t *testing.T) storageOperatorRequest {
				request, target := convergedSplitCity(t)
				if err := os.WriteFile(filepath.Join(target.Root, "sessions.db"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				return request
			},
			status: doctor.StatusWarning, want: "legacy per-class stores",
		},
		{
			name: "remote runtime without the attestation",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _ := convergedSplitCity(t)
				request.Cfg.Session.Provider = "k8s"
				return request
			},
			status: doctor.StatusError, want: "no --force=remote-runtime attestation",
		},
		{
			name: "remote runtime under the recorded attestation",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _ := convergedSplitCity(t)
				request.Cfg.Session.Provider = "k8s"
				recordStorageAttestation(request.CityPath, "test", []string{storageHazardRemoteRuntime}, io.Discard)
				return request
			},
			status: doctor.StatusWarning, want: "under the recorded --force=remote-runtime",
		},
		{
			name: "unsplit, provider up",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _, _ := splitReadyCity(t, 20, nativeDoltLane)
				refuseInfraMigrationSource(t)
				return request
			},
			status: doctor.StatusOK, want: "rehearses the split", detail: "provider: up",
		},
		{
			name: "unsplit, provider down, still OK until the default flips (O11)",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _, _ := splitReadyCity(t, 20, nativeDoltLane)
				refuseInfraMigrationSource(t)
				stubMigrationProviderDown(t, true)
				return request
			},
			status: doctor.StatusOK, want: "rehearses the split", detail: "is down",
		},
		{
			name: "unsplit with a cleared work store's note",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _, _ := splitReadyCity(t, 20, nativeDoltLane)
				if err := writeInfraClearedNote(request.CityPath, infraClearedNote{Binding: "infra", Complete: true}); err != nil {
					t.Fatal(err)
				}
				return request
			},
			status: doctor.StatusError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.setup(t)
			got := newStorageSplitCheck(request.Cfg, request.CityPath).Run(nil)
			if got.Status != tc.status || !strings.Contains(got.Message, tc.want) || !strings.Contains(strings.Join(got.Details, "\n"), tc.detail) {
				t.Fatalf("storage-split = %v %q, want %v containing %q; details %v, want %q", got.Status, got.Message, tc.status, tc.want, got.Details, tc.detail)
			}
			code := doStorageStatusCity(request, io.Discard, io.Discard)
			_, remote := cityRunsRemoteSessions(request.Cfg, request.CityPath)
			if !remote && (code != 0) != (got.Status == doctor.StatusError) {
				t.Errorf("status exits %d and doctor says %v: they disagree", code, got.Status)
			}
		})
	}
}

// TestStorageSplitDoctorHoldsTheVersionFloor: a floor above this gc or above
// the session PATH's gc is an error; a PATH gc whose version cannot be
// compared warns; no gc on the PATH is not a finding.
func TestStorageSplitDoctorHoldsTheVersionFloor(t *testing.T) {
	for _, tc := range []struct {
		name, self, path, floor string
		status                  doctor.CheckStatus
		want                    string
	}{
		{"this gc below", "1.5.0", "1.6.0", "1.5.1", doctor.StatusError, "this gc is 1.5.0"},
		{"the PATH gc below", "1.6.0", "1.4.2", "1.5.1", doctor.StatusError, "session PATH (/usr/local/bin/gc) is 1.4.2"},
		{"the PATH gc a dev build", "1.6.0", "dev", "1.5.1", doctor.StatusWarning, "cannot be compared"},
		{"no PATH gc", "1.6.0", "", "1.5.1", doctor.StatusOK, ""},
		{"both at the floor", "1.5.1", "1.5.1-rc.2", "1.5.1", doctor.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, _ := convergedSplitCity(t)
			stubGCVersion(t, tc.self)
			stubSessionPathGC(t, tc.path, nil)
			if err := writeMinGCVersion(request.CityPath, tc.floor); err != nil {
				t.Fatal(err)
			}
			got := newStorageSplitCheck(request.Cfg, request.CityPath).Run(nil)
			if got.Status != tc.status || !strings.Contains(got.Message, tc.want) {
				t.Errorf("storage-split = %v %q, want %v containing %q", got.Status, got.Message, tc.status, tc.want)
			}
		})
	}
}

// TestMinGCVersionRefusal pins the floor's comparison and its file.
func TestMinGCVersionRefusal(t *testing.T) {
	for _, tc := range []struct {
		running, floor string
		refused        bool
	}{
		{"1.5.0", "1.5.1", true},
		{"v1.4.9", "1.5.0", true},
		{"0.9.9", "1.0.0", true},
		{"1.5.1", "1.5.1", false},
		{"1.5.1-rc.1", "1.5.1", false},
		{"1.5.1+ra.1", "1.5.1", false},
		{"1.10.0", "1.9.0", false},
		{"2.0.0", "1.99.99", false},
		{"dev", "9.9.9", false},
	} {
		city := t.TempDir()
		if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeMinGCVersion(city, tc.floor); err != nil {
			t.Fatal(err)
		}
		if err := minGCVersionRefusal(city, tc.running); (err != nil) != tc.refused {
			t.Errorf("gc %s on floor %s: refusal %v, want refused %t", tc.running, tc.floor, err, tc.refused)
		}
	}
	city := t.TempDir()
	if err := minGCVersionRefusal(city, "0.0.1"); err != nil {
		t.Errorf("a city with no floor refused: %v", err)
	}
	for _, bad := range []string{"v1.5.1", "1.5", "1.5.1-rc.1", ""} {
		if err := writeMinGCVersion(city, bad); err == nil {
			t.Errorf("writeMinGCVersion(%q) accepted a floor that is not MAJOR.MINOR.PATCH", bad)
		}
	}
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(minGCVersionPath(city), []byte("not a version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := minGCVersionRefusal(city, "9.9.9"); err == nil {
		t.Error("an unreadable floor did not refuse")
	}
}

// TestStorageBootGateRefusesAGCBelowTheCityFloor: the gate every boot and
// one-shot class route passes refuses first, even on a city with no
// [storage].
func TestStorageBootGateRefusesAGCBelowTheCityFloor(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubGCVersion(t, "1.5.0")
	if _, err := storageBootGate(city, &config.City{}, "test", nil, io.Discard); err != nil {
		t.Fatalf("a city with no floor refused: %v", err)
	}
	if err := writeMinGCVersion(city, "1.5.1"); err != nil {
		t.Fatal(err)
	}
	_, err := storageBootGate(city, &config.City{}, "test", nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires gc 1.5.1 or later") {
		t.Fatalf("storageBootGate = %v, want the floor's refusal", err)
	}
	stubGCVersion(t, "1.5.1")
	if _, err := storageBootGate(city, &config.City{}, "test", nil, io.Discard); err != nil {
		t.Errorf("a gc at the floor refused: %v", err)
	}
}

// TestStatusAndBootAgreeOnAStaleMarker (M50): a marker whose binding database
// is gone refuses at boot, and status and doctor say so too.
func TestStatusAndBootAgreeOnAStaleMarker(t *testing.T) {
	request, target := convergedSplitCity(t)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(target.Database + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if state, err := readInfraConvergenceState(target); err != nil || state != infraConvergenceStale {
		t.Fatalf("state = %v, %v: the fixture is not stale", state, err)
	}
	_, bootErr := storageBootGate(request.CityPath, request.Cfg, "test", nil, io.Discard)
	code := doStorageStatusCity(request, io.Discard, io.Discard)
	got := newStorageSplitCheck(request.Cfg, request.CityPath).Run(nil)
	if bootErr == nil || code == 0 || got.Status != doctor.StatusError {
		t.Fatalf("boot %v, status exit %d, doctor %v: a stale marker must refuse in all three", bootErr, code, got.Status)
	}
}

// TestStorageSplitDoctorOpensNoWindow (S1): the unsplit verdict opens no
// store and registers no migration window, so a check doctor abandons on its
// timeout leaves nothing behind for the checks after it.
func TestStorageSplitDoctorOpensNoWindow(t *testing.T) {
	request, _, _ := splitReadyCity(t, 5, nativeDoltLane)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		t.Error("the doctor check opened the work store")
		return nil, errors.New("refused")
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	if got := newStorageSplitCheck(request.Cfg, request.CityPath).Run(nil); got.Status != doctor.StatusOK {
		t.Fatalf("storage-split = %v %q", got.Status, got.Message)
	}
	migrationWindows.Lock()
	active := migrationWindows.active
	migrationWindows.Unlock()
	if active != nil {
		t.Fatalf("the doctor check left a migration window registered on %s", active.cityPath)
	}
}

// writeFakeGC puts a gc reporting v in dir.
func writeFakeGC(t *testing.T, dir, v string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+v+"\necho second line\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProbeSessionPathGCVersion runs the real probe: the supervisor's
// expanded PATH (here ~/.local/bin, ahead of the process PATH), workspace.env's
// PATH over it, relative entries skipped, and the first line of the output.
func TestProbeSessionPathGCVersion(t *testing.T) {
	home, procPath, wsPath := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", procPath)
	if path, _, err := probeSessionPathGCVersion(&config.City{}); err != nil || path != "" {
		t.Fatalf("no gc anywhere: %q, %v", path, err)
	}
	writeFakeGC(t, procPath, "1.4.2")
	if path, v, err := probeSessionPathGCVersion(&config.City{}); err != nil || path != filepath.Join(procPath, "gc") || v != "1.4.2" {
		t.Fatalf("process PATH: %q %q %v", path, v, err)
	}
	local := writeFakeGC(t, filepath.Join(home, ".local", "bin"), "1.5.0")
	if path, v, err := probeSessionPathGCVersion(nil); err != nil || path != local || v != "1.5.0" {
		t.Fatalf("the supervisor's expanded PATH puts ~/.local/bin first: %q %q %v", path, v, err)
	}
	ws := writeFakeGC(t, wsPath, "1.6.0")
	cfg := &config.City{Workspace: config.Workspace{Env: map[string]string{"PATH": "relative/bin" + string(os.PathListSeparator) + wsPath}}}
	if path, v, err := probeSessionPathGCVersion(cfg); err != nil || path != ws || v != "1.6.0" {
		t.Fatalf("workspace.env PATH: %q %q %v", path, v, err)
	}
	// A relative entry that does hold a gc, from the probe's working
	// directory, is still not the session's: the session runs elsewhere.
	cwd := t.TempDir()
	writeFakeGC(t, filepath.Join(cwd, "relative", "bin"), "9.9.9")
	t.Chdir(cwd)
	cfg.Workspace.Env["PATH"] = "relative/bin"
	if path, _, err := probeSessionPathGCVersion(cfg); err != nil || path != "" {
		t.Fatalf("a workspace PATH of relative entries alone found %q (%v)", path, err)
	}
}

// TestReadMinGCVersionShapes: a leading v is accepted, and an empty file
// records no floor.
func TestReadMinGCVersionShapes(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body, floor string
		present     bool
	}{{"v1.5.1\n", "1.5.1", true}, {"1.5.1", "1.5.1", true}, {"", "", false}, {"\n  \n", "", false}} {
		if err := os.WriteFile(minGCVersionPath(city), []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		floor, present, err := readMinGCVersion(city)
		if err != nil || floor != tc.floor || present != tc.present {
			t.Errorf("%q: %q %t %v, want %q %t", tc.body, floor, present, err, tc.floor, tc.present)
		}
	}
}

// TestCityIDFloorBelowWarning names repair-sequence --id-floor, or for hosted
// bd the out-of-band setting (O10).
func TestCityIDFloorBelowWarning(t *testing.T) {
	if got := cityIDFloorBelowWarning(cityIDFloorPlan{Need: 7, Current: 5}); !strings.Contains(got, "gc storage repair-sequence --id-floor") {
		t.Errorf("local: %q", got)
	}
	if got := cityIDFloorBelowWarning(cityIDFloorPlan{Need: 7, Current: 5, Hosted: "https://h"}); !strings.Contains(got, "out of band (owner decision O10)") || !strings.Contains(got, "max_hash_length at least 7") {
		t.Errorf("hosted: %q", got)
	}
}

// TestStorageSplitDoctorRows reaches each remaining row of the check: a floor
// under the carried ids (M33), a rig awaiting import (M34), a legacy combined
// store (M36), a remote stage (M40) and a PATH gc that cannot report its
// version (M39).
func TestStorageSplitDoctorRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T) storageOperatorRequest
		status doctor.CheckStatus
		want   string
	}{
		{
			name: "floor below the carried ids",
			setup: func(t *testing.T) storageOperatorRequest {
				cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
				request := storageTestRequest(t, cfg)
				source := stubInfraMigrationSource(t).(*beads.MemStore)
				source.HonorExplicitIDs = true
				stubInfraControllerPing(t, 0)
				mustCreateInfraBead(t, source, beads.Bead{ID: "gc-abcd", Title: "a session", Type: "session"})
				var log bytes.Buffer
				if got := migrateInfraClasses(t, request.CityPath, cfg, &log); got.Outcome != infraMigrationConverged {
					t.Fatalf("migrating: %s: %s", got.Outcome, log.String())
				}
				stubCityHashConfig(t, cityHashConfig{Conf: &fakeHashConfig{values: map[string]string{bdMinHashLengthKey: "3"}}, Prefix: "gc"})
				return request
			},
			status: doctor.StatusWarning, want: "is 3, below the 5 the carried ids need: run `gc storage repair-sequence --id-floor`",
		},
		{
			name: "a rig awaiting import",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _ := convergedSplitCity(t)
				rigPath := t.TempDir()
				request.Cfg.Rigs = []config.Rig{{Name: "alpha", Prefix: "ga", Path: rigPath}}
				rig := beads.NewMemStore()
				rig.IDPrefix = "ga"
				mustCreateInfraBead(t, rig, beads.Bead{Title: "a session", Type: "session"})
				prev := openStorageScopeStore
				openStorageScopeStore = func(storePath, cityPath string) (beads.Store, error) {
					if storePath == rigPath {
						return rig, nil
					}
					return prev(storePath, cityPath)
				}
				t.Cleanup(func() { openStorageScopeStore = prev })
				return request
			},
			status: doctor.StatusWarning, want: "1 rig(s) await import",
		},
		{
			name: "a legacy combined store",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _ := convergedSplitCity(t)
				if err := os.MkdirAll(filepath.Join(request.CityPath, ".gc", "infra", ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				return request
			},
			status: doctor.StatusWarning, want: "legacy combined infrastructure store",
		},
		{
			name: "a remote stage",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _ := convergedSplitCity(t)
				if err := writeRemoteStage(request.CityPath, remoteStage{ControllerCity: "hq", ControllerPath: "/srv/hq"}); err != nil {
					t.Fatal(err)
				}
				return request
			},
			status: doctor.StatusOK, want: "remote stage of hq (/srv/hq)",
		},
		{
			name: "a PATH gc that cannot report its version",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _ := convergedSplitCity(t)
				stubGCVersion(t, "1.6.0")
				stubSessionPathGC(t, "", errors.New("exit status 2"))
				if err := writeMinGCVersion(request.CityPath, "1.5.1"); err != nil {
					t.Fatal(err)
				}
				return request
			},
			status: doctor.StatusWarning, want: "could not report its version (exit status 2)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.setup(t)
			got := newStorageSplitCheck(request.Cfg, request.CityPath).Run(nil)
			if got.Status != tc.status || !strings.Contains(got.Message, tc.want) {
				t.Errorf("storage-split = %v %q, want %v containing %q; details %v", got.Status, got.Message, tc.status, tc.want, got.Details)
			}
		})
	}
}
