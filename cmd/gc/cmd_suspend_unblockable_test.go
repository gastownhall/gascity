package main

import (
	"bytes"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// suspendDeadline is how long `gc suspend` may take against an API that
// never answers.
const suspendDeadline = 5 * time.Second

// hangingAPI is a listener that accepts every connection and never writes a
// byte back, as a wedged API server does.
func hangingAPI(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close() //nolint:errcheck // test cleanup
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close() //nolint:errcheck // test cleanup
		}
	})
	return "http://" + ln.Addr().String()
}

// routeToHangingAPI routes the city's API client to a server that never
// answers.
func routeToHangingAPI(t *testing.T, cityName string) {
	t.Helper()
	url := hangingAPI(t)
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteControllerIdentityHook, apiRouteSupervisorClientHook = origIdentity, origSup })
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{} }
	apiRouteSupervisorClientHook = func(string) *api.Client { return api.NewCityScopedClient(url, cityName) }
}

// AC-3: against an API that accepts connections but never responds, `gc
// suspend` exits 0 within 5 seconds with the city recorded as suspended: with
// a good city.toml, with one that does not parse (a warning), and over a
// malformed suspension file (replaced, its parse error printed).
func TestSuspendCannotBeBlockedByAHangingAPI(t *testing.T) {
	t.Setenv("GC_NO_API", "")
	for _, tc := range []struct {
		name       string
		cityTOML   string
		suspension string
		stderr     []string
	}{
		{name: "good config", cityTOML: "[workspace]\nname = \"hang\"\n"},
		{name: "config does not parse", cityTOML: "[workspace\nname = ", stderr: []string{"warning"}},
		{name: "malformed suspension file", cityTOML: "[workspace]\nname = \"hang\"\n", suspension: `{"city":{"susp`, stderr: []string{"replaced", "unexpected end of JSON input"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.WriteFile(cityPath+"/city.toml", []byte(tc.cityTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.suspension != "" {
				writeSuspensionFile(t, cityPath, tc.suspension)
			}
			routeToHangingAPI(t, "hang")
			var stdout, stderr bytes.Buffer
			start := time.Now()
			code := cmdSuspend([]string{cityPath}, false, &stdout, &stderr)
			if took := time.Since(start); took >= suspendDeadline {
				t.Fatalf("gc suspend took %s against a hanging API, want under %s", took, suspendDeadline)
			} else if took < suspendAPITimeout-time.Second {
				t.Fatalf("gc suspend took %s, want about %s: the API route was not attempted before the direct write", took, suspendAPITimeout)
			}
			if code != 0 {
				t.Fatalf("gc suspend exit %d, want 0; stderr: %s", code, stderr.String())
			}
			st, err := suspensionstate.Load(fsys.OSFS{}, cityPath)
			if err != nil {
				t.Fatalf("suspension file after gc suspend: %v", err)
			}
			if !suspensionstate.IsCitySuspended(st) {
				t.Fatalf("suspension file %s does not record the city suspended: %+v", citylayout.SuspensionStateFile(cityPath), st)
			}
			for _, want := range tc.stderr {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr %q does not contain %q", stderr.String(), want)
				}
			}
		})
	}
}

// AC-3: `gc resume` still refuses a city.toml that does not parse and a
// malformed suspension file, naming the remedy, and leaves the file as it was.
func TestResumeRefusesBrokenConfigAndMalformedSuspensionFile(t *testing.T) {
	for _, tc := range []struct {
		name, cityTOML, suspension, remedy string
	}{
		{name: "config does not parse", cityTOML: "[workspace\nname = ", remedy: "city.toml"},
		{name: "malformed suspension file", cityTOML: "[workspace]\nname = \"r\"\n", suspension: `{"city":`, remedy: "remove the file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.WriteFile(cityPath+"/city.toml", []byte(tc.cityTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.suspension != "" {
				writeSuspensionFile(t, cityPath, tc.suspension)
			}
			var stdout, stderr bytes.Buffer
			if code := doSuspendCity(fsys.OSFS{}, cityPath, false, false, &stdout, &stderr); code == 0 {
				t.Fatalf("gc resume exit 0, want a refusal; stdout: %s", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.remedy) {
				t.Fatalf("stderr %q does not name the remedy %q", stderr.String(), tc.remedy)
			}
			if tc.suspension != "" {
				got, err := os.ReadFile(citylayout.SuspensionStateFile(cityPath))
				if err != nil || string(got) != tc.suspension {
					t.Fatalf("suspension file = %q (%v), want it left as %q", got, err, tc.suspension)
				}
			}
		})
	}
}
