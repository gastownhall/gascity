package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSupervisorAddrInUse verifies that only EADDRINUSE (the signature of a
// second supervisor competing for the shared API port) is classified as a
// port collision — other listen errors must fall through to the generic path.
func TestSupervisorAddrInUse(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"address in use", syscall.EADDRINUSE, true},
		{"wrapped address in use", &net.OpError{Op: "listen", Err: syscall.EADDRINUSE}, true},
		{"connection refused", syscall.ECONNREFUSED, false},
		{"permission denied", syscall.EACCES, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorAddrInUse(tc.err); got != tc.want {
				t.Fatalf("supervisorAddrInUse(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSupervisorPortInUseMessage verifies the diagnostic is loud and
// actionable: it names the address, states only one supervisor may run, and
// points the operator at both remedies (stop the other / pick another port).
func TestSupervisorPortInUseMessage(t *testing.T) {
	const addr = "127.0.0.1:8372"
	const cfg = "/home/someone/.gc/supervisor.toml"
	msg := supervisorPortInUseMessage(addr, cfg)

	for _, want := range []string{
		addr,
		cfg,
		"already in use",
		"only one supervisor",
		"gc supervisor stop",
		"port =",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("port-in-use message missing %q; got:\n%s", want, msg)
		}
	}
}

// TestSupervisorPortInUseMessageDarwinDoesNotClaimWithoutRestart verifies
// the MAJOR-A fix: launchd's KeepAlive has no per-exit-code equivalent of
// systemd's RestartPreventExitStatus, so a duplicate supervisor on macOS is
// still restarted regardless of exit code. Claiming "without restart" there
// would be unconditionally false.
func TestSupervisorPortInUseMessageDarwinDoesNotClaimWithoutRestart(t *testing.T) {
	old := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = old })

	msg := supervisorPortInUseMessage("127.0.0.1:8372", "/home/someone/.gc/supervisor.toml")
	if strings.Contains(msg, "without restart") {
		t.Errorf("darwin port-in-use message falsely claims restart suppression; got:\n%s", msg)
	}
	if !strings.Contains(msg, "launchd") || !strings.Contains(msg, "regardless of exit code") {
		t.Errorf("darwin port-in-use message should explain launchd's restart-regardless-of-exit-code behavior; got:\n%s", msg)
	}
}

// TestSupervisorPortInUseMessageLinuxClaimsWithoutRestart is the systemd-side
// counterpart: RestartPreventExitStatus genuinely stops the restart there, so
// the message should say so.
func TestSupervisorPortInUseMessageLinuxClaimsWithoutRestart(t *testing.T) {
	old := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "linux"
	t.Cleanup(func() { supervisorRuntimeGOOS = old })

	msg := supervisorPortInUseMessage("127.0.0.1:8372", "/home/someone/.gc/supervisor.toml")
	if !strings.Contains(msg, "without restart") {
		t.Errorf("linux port-in-use message should claim restart suppression; got:\n%s", msg)
	}
}

// TestSupervisorRespondingGCSupervisor verifies the /health liveness gate
// used to discriminate a true duplicate gc supervisor (which answers
// {"status":"ok"}) from a foreign or non-responding binder that merely
// happens to hold the shared API port (gc-r0k40, MAJOR-B).
func TestSupervisorRespondingGCSupervisor(t *testing.T) {
	t.Run("responding gc supervisor", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/health" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"ok","version":"1.2.3"}`) //nolint:errcheck
		}))
		defer srv.Close()

		addr := strings.TrimPrefix(srv.URL, "http://")
		if !supervisorRespondingGCSupervisor(addr) {
			t.Errorf("supervisorRespondingGCSupervisor(%q) = false, want true", addr)
		}
	})

	t.Run("foreign HTTP responder", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"hello":"world"}`) //nolint:errcheck
		}))
		defer srv.Close()

		addr := strings.TrimPrefix(srv.URL, "http://")
		if supervisorRespondingGCSupervisor(addr) {
			t.Errorf("supervisorRespondingGCSupervisor(%q) = true, want false (not a gc supervisor payload)", addr)
		}
	})

	t.Run("non-2xx responder", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		addr := strings.TrimPrefix(srv.URL, "http://")
		if supervisorRespondingGCSupervisor(addr) {
			t.Errorf("supervisorRespondingGCSupervisor(%q) = true, want false (non-2xx)", addr)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := lis.Addr().String()
		if err := lis.Close(); err != nil {
			t.Fatal(err)
		}
		if supervisorRespondingGCSupervisor(addr) {
			t.Errorf("supervisorRespondingGCSupervisor(%q) = true, want false (connection refused)", addr)
		}
	})

	t.Run("timeout is bounded", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer lis.Close() //nolint:errcheck

		old := supervisorHealthProbeTimeout
		supervisorHealthProbeTimeout = 100 * time.Millisecond
		t.Cleanup(func() { supervisorHealthProbeTimeout = old })

		addr := lis.Addr().String()
		start := time.Now()
		if supervisorRespondingGCSupervisor(addr) {
			t.Errorf("supervisorRespondingGCSupervisor(%q) = true, want false (unaccepted connection)", addr)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("supervisorRespondingGCSupervisor took %s, want bounded by supervisorHealthProbeTimeout", elapsed)
		}
	})
}

// TestSupervisorSystemdTemplatePreventsRestartOnPortCollision verifies the
// generated systemd unit lists the duplicate-port exit code in
// RestartPreventExitStatus, so a duplicate install exits once instead of
// crash-looping on the shared port forever (the ga-ceq regression).
func TestSupervisorSystemdTemplatePreventsRestartOnPortCollision(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:            "/usr/local/bin/gc",
		LogPath:           "/home/someone/.gc/supervisor.log",
		GCHome:            "/home/someone/.gc",
		Path:              "/usr/bin",
		PortInUseExitCode: supervisorExitCodePortInUse,
	}
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatalf("render systemd template: %v", err)
	}
	want := "RestartPreventExitStatus=" + strconv.Itoa(supervisorExitCodePortInUse)
	if !strings.Contains(content, want) {
		t.Fatalf("systemd unit missing %q; got:\n%s", want, content)
	}
	// Genuine crashes must still restart — Restart=always stays in force.
	if !strings.Contains(content, "Restart=always") {
		t.Fatalf("systemd unit lost Restart=always; got:\n%s", content)
	}
}
