package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			name: "unsplit and ready",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _, _ := splitReadyCity(t, 20, nativeDoltLane)
				return request
			},
			status: doctor.StatusOK, want: "rehearses the split: ready",
		},
		{
			name: "unsplit and refused, still OK until the default flips (O11)",
			setup: func(t *testing.T) storageOperatorRequest {
				request, _, _ := splitReadyCity(t, 20, nativeDoltLane)
				stubSplitPreflightLegs(t, []v2SessionLeg{{ref: "city", notChecked: "open failed"}})
				return request
			},
			status: doctor.StatusOK, want: "it would refuse (v2 sessions)",
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
			if got.Status != tc.status || !strings.Contains(got.Message, tc.want) {
				t.Fatalf("storage-split = %v %q, want %v containing %q; details %v", got.Status, got.Message, tc.status, tc.want, got.Details)
			}
			if request.Cfg.Storage == nil {
				return
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
