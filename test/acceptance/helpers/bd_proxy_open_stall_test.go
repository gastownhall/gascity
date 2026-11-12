package acceptancehelpers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two lines are the acceptance gate's own output (ga-gcasrs run c3,
// 2026-10-10): TestBeadsProxiedDefaultNativeLane's and
// TestProxiedIdleTimeoutReapAndTransparentRestart's failed gc init.
const (
	gateStallLine          = "gc init: provider-owned beads init: Error: failed to open uow provider: uow: get proxy endpoint: timeout waiting for proxy to become ready on its OS-assigned port"
	gateStallKillFailLine  = "gc init: provider-owned beads init: Error: failed to open uow provider: uow: get proxy endpoint: timeout waiting for proxy on its OS-assigned port; safe child kill failed: timeout (5s) waiting for pid 403"
	gateInitOutputPrefix   = "[1/8] Creating runtime scaffold\nInitialized city \"at-936e28c5\" with default provider \"claude\".\n[6/8] Skipping provider readiness checks\n"
	gateInitOutputHintLine = "\nhint: run \"gc doctor\" for diagnostics\n"
	postFixStallLine       = "gc init: provider-owned beads init: Error: failed to open uow provider: uow: get proxy endpoint: timeout waiting for proxy to become ready on its OS-assigned port: proxy child still running at the open deadline (child pid 403, started 45.1s ago; open budget 45s; see /c/.beads/dolt/server.log)"
)

func TestIsBDProxyOpenStall(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"gate stall", gateInitOutputPrefix + gateStallLine + gateInitOutputHintLine, true},
		{"gate stall with an unconfirmed kill", gateInitOutputPrefix + gateStallKillFailLine + gateInitOutputHintLine, true},
		// What a bd carrying be-q6uduz says for the same stall (beads
		// childTimeoutError): the phrase stays, details follow it, and the
		// recovery no longer applies.
		{"stall from a bd with the open budget", gateInitOutputPrefix + postFixStallLine + gateInitOutputHintLine, false},
		{"stall from a bd with the open budget, kill unconfirmed", gateInitOutputPrefix + postFixStallLine + "; safe child kill failed: timeout (5s) waiting for pid 403" + gateInitOutputHintLine, false},
		{"proxy child exited: a real error", gateInitOutputPrefix + "gc init: provider-owned beads init: Error: failed to open uow provider: uow: get proxy endpoint: proxy child exited before publishing its OS-assigned port (see /c/.beads/dolt/server.log): exit status 1", false},
		{"lost port, the beads#7184 class", gateInitOutputPrefix + "gc init: provider-owned beads init: Error: failed to open uow provider: uow: ping db: dial tcp 127.0.0.1:42021: connect: connection refused", false},
		{"gc's own init deadline", "gc init: provider-owned beads init: context deadline exceeded", false},
		{"stall text outside the init step", "Error: failed to open uow provider: uow: get proxy endpoint: timeout waiting for proxy to become ready on its OS-assigned port", false},
		{"unrelated failure", "gc init: loading config for prefix resolution: bad toml", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBDProxyOpenStall(tc.out); got != tc.want {
				t.Errorf("isBDProxyOpenStall() = %v, want %v for:\n%s", got, tc.want, tc.out)
			}
		})
	}
}

func TestInitRecoveringBDProxyOpenStall(t *testing.T) {
	stallOut := gateInitOutputPrefix + gateStallLine + gateInitOutputHintLine
	errExit := errors.New("exit status 1")

	// run drives initRecoveringBDProxyOpenStall over scripted attempts and
	// records every call it makes, in order.
	run := func(attempts []error, outs []string, armed bool, resetErr error) (string, []string, error) {
		var calls []string
		n := 0
		out, err := initRecoveringBDProxyOpenStall(
			func() (string, error) {
				calls = append(calls, "init")
				i := n
				n++
				return outs[i], attempts[i]
			},
			func() bool { calls = append(calls, "armed"); return armed },
			func() error { calls = append(calls, "reset"); return resetErr },
			func(string) { calls = append(calls, "report") },
		)
		return out, calls, err
	}

	t.Run("retries once from a reset city", func(t *testing.T) {
		out, calls, err := run([]error{errExit, nil}, []string{stallOut, "ok"}, true, nil)
		if err != nil || out != "ok" {
			t.Fatalf("got (%q, %v), want (ok, nil)", out, err)
		}
		if got := strings.Join(calls, ","); got != "init,armed,report,reset,init" {
			t.Fatalf("calls = %s, want init,armed,report,reset,init", got)
		}
	})
	t.Run("returns the retry's failure and stops there", func(t *testing.T) {
		second := errors.New("exit status 1 (second)")
		out, calls, err := run([]error{errExit, second}, []string{stallOut, stallOut + "again"}, true, nil)
		if !errors.Is(err, second) || out != stallOut+"again" {
			t.Fatalf("got (%q, %v), want the second attempt's result", out, err)
		}
		if got := strings.Join(calls, ","); got != "init,armed,report,reset,init" {
			t.Fatalf("calls = %s, want exactly one retry", got)
		}
	})
	t.Run("does not retry another failure", func(t *testing.T) {
		_, calls, err := run([]error{errExit}, []string{"Error: database not found"}, true, nil)
		if !errors.Is(err, errExit) || strings.Join(calls, ",") != "init" {
			t.Fatalf("got err %v after calls %v, want the first failure after init alone", err, calls)
		}
	})
	t.Run("does not retry a success that mentions the text", func(t *testing.T) {
		_, calls, err := run([]error{nil}, []string{stallOut}, true, nil)
		if err != nil || strings.Join(calls, ",") != "init" {
			t.Fatalf("got err %v after calls %v, want success after init alone", err, calls)
		}
	})
	t.Run("does not retry when disarmed", func(t *testing.T) {
		out, calls, err := run([]error{errExit}, []string{stallOut}, false, nil)
		if !errors.Is(err, errExit) || out != stallOut || strings.Join(calls, ",") != "init,armed" {
			t.Fatalf("got (%q, %v) after calls %v, want the first failure, unretried", out, err, calls)
		}
	})
	t.Run("a reset error fails without a retry", func(t *testing.T) {
		resetErr := errors.New("still running")
		_, calls, err := run([]error{errExit}, []string{stallOut}, true, resetErr)
		if !errors.Is(err, errExit) || !errors.Is(err, resetErr) || strings.Join(calls, ",") != "init,armed,report,reset" {
			t.Fatalf("got err %v after calls %v, want both errors and no second init", err, calls)
		}
	})
}

func TestBDScopePIDs(t *testing.T) {
	const city = "/tmp/gc-acceptance-1/at-0123abcd"
	// ps -eo pid=,args= shapes, as the gate's own cleanup listed them.
	table := strings.Join([]string{
		"    1 /sbin/init",
		"  403 /cache/bd db-proxy-child --root " + city + "/.beads/dolt --port 0 --idle-timeout 30m0s --backend local-server",
		"  404 /tmp/gc-acceptance-2/bin/dolt sql-server --config " + city + "/.beads/dolt/config.yaml",
		"  405 /tmp/gc-acceptance-2/bin/dolt sql-server --config " + city + "-other/.beads/dolt/config.yaml",
		"  406 /usr/bin/tail -f " + city + "/.beads/dolt/server.log",
		"  407 [dolt] <defunct>",
		"",
	}, "\n")
	pids, err := bdScopePIDs(table, city)
	if err != nil {
		t.Fatalf("bdScopePIDs: %v", err)
	}
	if got := fmt.Sprint(pids); got != "[403 404]" {
		t.Fatalf("bdScopePIDs() = %s, want [403 404]: the proxy and dolt under the city, not a sibling directory's dolt or another process naming the city", got)
	}
	if _, err := bdScopePIDs("abc /bin/dolt sql-server --config "+city+"/.beads/dolt/config.yaml", city); err == nil {
		t.Fatal("bdScopePIDs() = nil error for an unparseable pid")
	}
}

func TestResetBDProxiedCityEmptiesTheDirectory(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "at-0123abcd")
	for _, dir := range []string{filepath.Join(cityDir, ".beads", "dolt"), filepath.Join(cityDir, ".gc")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(cityDir, "city.toml"), filepath.Join(cityDir, ".beads", "dolt", "config.yaml")} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetBDProxiedCity(cityDir); err != nil {
		t.Fatalf("resetBDProxiedCity: %v", err)
	}
	if !dirIsEmpty(cityDir) {
		entries, _ := os.ReadDir(cityDir)
		t.Fatalf("city directory not empty after reset: %v", entries)
	}
}
