package main

import (
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveDoltPort_FlagWins(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")
	in := PortResolverInput{
		Flag:     "9999",
		CityPort: 4242,
		Rigs:     []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:       fs,
	}

	got := ResolveDoltPort(in)

	if got.Port != 9999 {
		t.Errorf("Port = %d, want 9999", got.Port)
	}
	if got.Fallback {
		t.Errorf("Fallback = true, want false")
	}
	if got.Source != "--port flag" {
		t.Errorf("Source = %q, want %q", got.Source, "--port flag")
	}
}

func TestResolveDoltPort_FlagInvalidFallsThrough(t *testing.T) {
	fs := fsys.NewFake()
	in := PortResolverInput{
		Flag:     "not-a-number",
		CityPort: 4242,
		FS:       fs,
	}

	got := ResolveDoltPort(in)

	if got.Port != 4242 {
		t.Errorf("Port = %d, want 4242 (city config fallback)", got.Port)
	}
	if got.Source != "city config dolt.port" {
		t.Errorf("Source = %q, want %q", got.Source, "city config dolt.port")
	}
	// First attempt should record the parse error.
	if len(got.Tried) == 0 || got.Tried[0].Status != "error" {
		t.Errorf("expected first attempt to record error, got %+v", got.Tried)
	}
}

func TestResolveDoltPort_CityConfigBeatsRigFile(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")
	in := PortResolverInput{
		CityPort: 4242,
		Rigs:     []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:       fs,
	}

	got := ResolveDoltPort(in)

	if got.Port != 4242 {
		t.Errorf("Port = %d, want 4242", got.Port)
	}
	if got.Source != "city config dolt.port" {
		t.Errorf("Source = %q, want city config dolt.port", got.Source)
	}
}

func TestResolveDoltPort_HQRigPortFileWins(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")
	fs.Files["/elsewhere/.beads/dolt-server.port"] = []byte("19999\n")
	in := PortResolverInput{
		Rigs: []resolverRig{
			{Name: "ext", Path: "/elsewhere", HQ: false},
			{Name: "hq", Path: "/city", HQ: true},
		},
		FS: fs,
	}

	got := ResolveDoltPort(in)

	if got.Port != 28231 {
		t.Errorf("Port = %d, want 28231 (HQ rig)", got.Port)
	}
	if got.Source != "/city/.beads/dolt-server.port" {
		t.Errorf("Source = %q, want HQ port-file path", got.Source)
	}
	if got.Fallback {
		t.Errorf("Fallback = true, want false")
	}
}

func TestResolveDoltPort_NonHQRigUsedWhenHQAbsent(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/elsewhere/.beads/dolt-server.port"] = []byte("19999\n")
	in := PortResolverInput{
		Rigs: []resolverRig{
			{Name: "ext", Path: "/elsewhere", HQ: false},
			{Name: "hq", Path: "/city", HQ: true},
		},
		FS: fs,
	}

	got := ResolveDoltPort(in)

	if got.Port != 19999 {
		t.Errorf("Port = %d, want 19999 (non-HQ rig)", got.Port)
	}
	if got.Source != "/elsewhere/.beads/dolt-server.port" {
		t.Errorf("Source = %q, want non-HQ port-file path", got.Source)
	}
}

func TestResolveDoltPort_LegacyFallbackWhenNothingResolves(t *testing.T) {
	fs := fsys.NewFake()
	in := PortResolverInput{
		Rigs: []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:   fs,
	}

	got := ResolveDoltPort(in)

	if got.Port != 3307 {
		t.Errorf("Port = %d, want 3307 (legacy default)", got.Port)
	}
	if !got.Fallback {
		t.Errorf("Fallback = false, want true")
	}
	if got.Source != "legacy default" {
		t.Errorf("Source = %q, want legacy default", got.Source)
	}
}

func TestResolveDoltPort_TriedRecordsAllSources(t *testing.T) {
	fs := fsys.NewFake()
	in := PortResolverInput{
		Rigs: []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:   fs,
	}

	got := ResolveDoltPort(in)

	if len(got.Tried) < 4 {
		t.Fatalf("Tried = %d entries, want at least 4 (flag, config, rig file, legacy)", len(got.Tried))
	}
	wantSources := []string{
		"--port flag",
		"city config dolt.port",
		"/city/.beads/dolt-server.port",
		"legacy default",
	}
	for i, want := range wantSources {
		if got.Tried[i].Source != want {
			t.Errorf("Tried[%d].Source = %q, want %q", i, got.Tried[i].Source, want)
		}
	}
}

func TestResolveDoltPort_BadRigPortFileStopsBeforeLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*fsys.Fake)
		wantDetail string
	}{
		{
			name:       "empty",
			setup:      func(fs *fsys.Fake) { fs.Files["/city/.beads/dolt-server.port"] = []byte("\n") },
			wantDetail: "empty",
		},
		{
			name:       "malformed",
			setup:      func(fs *fsys.Fake) { fs.Files["/city/.beads/dolt-server.port"] = []byte("not-a-port\n") },
			wantDetail: "invalid port",
		},
		{
			name:       "out of range",
			setup:      func(fs *fsys.Fake) { fs.Files["/city/.beads/dolt-server.port"] = []byte("70000\n") },
			wantDetail: "must be between 1 and 65535",
		},
		{
			name:       "unreadable",
			setup:      func(fs *fsys.Fake) { fs.Errors["/city/.beads/dolt-server.port"] = os.ErrPermission },
			wantDetail: "permission",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			tc.setup(fs)
			in := PortResolverInput{
				Rigs: []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
				FS:   fs,
			}

			got := ResolveDoltPort(in)

			if got.Port != 0 {
				t.Errorf("Port = %d, want unresolved zero port", got.Port)
			}
			if got.Fallback {
				t.Errorf("Fallback = true, want false for bad rig port file")
			}
			if got.Source != "/city/.beads/dolt-server.port" {
				t.Errorf("Source = %q, want bad rig-port-file path", got.Source)
			}
			for _, attempt := range got.Tried {
				if attempt.Source == "legacy default" {
					t.Fatalf("legacy default was tried after bad rig port file: %+v", got.Tried)
				}
				if attempt.Source == "/city/.beads/dolt-server.port" {
					if attempt.Status != "error" {
						t.Errorf("rig-port-file attempt status = %q, want error", attempt.Status)
					}
					if !strings.Contains(attempt.Detail, tc.wantDetail) {
						t.Errorf("rig-port-file detail = %q, want substring %q", attempt.Detail, tc.wantDetail)
					}
					return
				}
			}
			t.Errorf("did not find /city/.beads/dolt-server.port in Tried entries: %+v", got.Tried)
		})
	}
}

func TestResolveDoltPort_NoRigsFalse_FallsThroughDirectly(t *testing.T) {
	fs := fsys.NewFake()
	in := PortResolverInput{
		FS: fs,
	}

	got := ResolveDoltPort(in)

	if got.Port != 3307 || !got.Fallback {
		t.Errorf("expected legacy fallback with no rigs, got %+v", got)
	}
}

func TestResolveDoltPort_FlagZeroRejected(t *testing.T) {
	fs := fsys.NewFake()
	in := PortResolverInput{
		Flag:     "0",
		CityPort: 4242,
		FS:       fs,
	}

	got := ResolveDoltPort(in)

	if got.Port == 0 {
		t.Errorf("Port = 0; resolver must reject a zero --port and fall through")
	}
	if got.Source != "city config dolt.port" {
		t.Errorf("Source = %q, want city-config fallback after zero flag", got.Source)
	}
}
