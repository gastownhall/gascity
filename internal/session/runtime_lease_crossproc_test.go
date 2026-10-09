package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The cross-process race harness (ARCH-RESTRUCTURE R6.4). The test binary
// re-execs itself in a lease role: the processes share the SQLite store dir,
// and the city dir too when they are on one host. A second host is simulated
// by a separate city dir (its own lock namespace) and a replaced hostname.
// The role reports on stdout and waits for a line on stdin to proceed.

const leaseRoleEnv = "GC_TEST_ROLE"

// TestRuntimeLeaseHelperProcess is the re-exec'd role, a no-op otherwise.
//   - hold: acquire, wait to proceed, release.
//   - stall: acquire, wait to proceed, then write under the lease, as a
//     start's late commit does.
func TestRuntimeLeaseHelperProcess(t *testing.T) {
	role := os.Getenv(leaseRoleEnv)
	if role == "" {
		return
	}
	if host := os.Getenv("GC_TEST_LEASE_HOST"); host != "" {
		runtimeLeaseHostname = func() (string, error) { return host, nil }
	}
	store := openLeaseStore(t, os.Getenv("GC_TEST_LEASE_STORE"))
	ttl, _ := time.ParseDuration(os.Getenv("GC_TEST_LEASE_TTL"))
	l, err := TryRuntimeLease(NewStore(beads.SessionStore{Store: store}), RuntimeLeaseRequest{
		City: os.Getenv("GC_TEST_LEASE_CITY"), Name: "s-lease", ID: os.Getenv("GC_TEST_LEASE_ID"), TTL: ttl,
	})
	if err != nil {
		fmt.Printf("refused %v\n", err)
		return
	}
	fmt.Printf("acquired %d\n", l.Epoch())
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	if role == "stall" {
		_, err := l.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch {
			return MetadataPatch{"state": "stale-commit"}
		})
		fmt.Printf("commit lost=%v err=%v\n", errors.Is(err, ErrRuntimeLeaseLost), err)
	}
	l.Release()
	fmt.Println("released")
}

// leaseRole is a running role process.
type leaseRole struct {
	cmd   *exec.Cmd
	lines chan string
	stdin io.WriteCloser
}

// expect waits for the role's next report to start with want.
func (r *leaseRole) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case line, ok := <-r.lines:
		if !ok || !strings.HasPrefix(line, want) {
			t.Fatalf("role reported %q (open=%v), want %q", line, ok, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("role never reported %q", want)
	}
}

func (r *leaseRole) proceed(t *testing.T) {
	t.Helper()
	if _, err := io.WriteString(r.stdin, "go\n"); err != nil {
		t.Fatal(err)
	}
}

// crash kills the role by its PID, as a crash mid-start would end it.
func (r *leaseRole) crash(t *testing.T) {
	t.Helper()
	if err := r.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = r.cmd.Wait()
}

// leaseHarness is a shared store holding one session row.
type leaseHarness struct {
	storeDir string
	f        leaseFixture
}

func newLeaseHarness(t *testing.T) leaseHarness {
	t.Helper()
	dir := t.TempDir()
	store := openLeaseStore(t, dir)
	created := seedPatchFenceSession(t, store, "s-lease")
	return leaseHarness{storeDir: dir, f: leaseFixture{store: store, front: NewStore(beads.SessionStore{Store: store}), id: created.ID}}
}

func (h leaseHarness) try(city string) (*RuntimeLease, error) {
	return TryRuntimeLease(h.f.front, RuntimeLeaseRequest{City: city, Name: "s-lease", ID: h.f.id, TTL: leaseTTL})
}

// TestRuntimeLeaseCrossProcess runs real processes against one store.
func TestRuntimeLeaseCrossProcess(t *testing.T) {
	// start runs role as a process on host ("" is this host) in city's lock
	// namespace, and waits for it to acquire.
	start := func(t *testing.T, h leaseHarness, role, host, city string, ttl time.Duration) *leaseRole {
		t.Helper()
		r := &leaseRole{lines: make(chan string, 8)}
		r.cmd = exec.Command(os.Args[0], "-test.run=^TestRuntimeLeaseHelperProcess$", "-test.count=1")
		r.cmd.Env = append(os.Environ(), leaseRoleEnv+"="+role, "GC_TEST_LEASE_HOST="+host,
			"GC_TEST_LEASE_STORE="+h.storeDir, "GC_TEST_LEASE_CITY="+city, "GC_TEST_LEASE_ID="+h.f.id,
			"GC_TEST_LEASE_TTL="+ttl.String())
		out, err := r.cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if r.stdin, err = r.cmd.StdinPipe(); err != nil {
			t.Fatal(err)
		}
		if err := r.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.cmd.Process.Kill(); _ = r.cmd.Wait() })
		go func() {
			for sc := bufio.NewScanner(out); sc.Scan(); {
				r.lines <- sc.Text()
			}
			close(r.lines)
		}()
		r.expect(t, "acquired 1")
		return r
	}

	// A process holding the lease excludes another on its host until it
	// releases.
	t.Run("two processes on one host", func(t *testing.T) {
		h, city := newLeaseHarness(t), t.TempDir()
		holder := start(t, h, "hold", "", city, leaseTTL)
		if _, err := h.try(city); !errors.Is(err, ErrRuntimeLeaseBusy) || !strings.Contains(err.Error(), "on this host") {
			t.Fatalf("contender while held = %v, want a local busy", err)
		}
		holder.proceed(t)
		holder.expect(t, "released")
		l, err := h.try(city)
		if err != nil || l.Epoch() != 2 {
			t.Fatalf("contender after release: %v, %v", err, l)
		}
		l.Release()
	})

	// A holder killed mid-start frees the lease at once, long before its
	// record expires.
	t.Run("crash releases on one host", func(t *testing.T) {
		h, city := newLeaseHarness(t), t.TempDir()
		start(t, h, "hold", "", city, leaseTTL).crash(t)
		if m := h.f.meta(t); m[RuntimeLeaseHolderKey] == "" {
			t.Fatalf("the crashed holder's record is gone: %v", m)
		}
		l, err := h.try(city)
		if err != nil || l.Epoch() != 2 {
			t.Fatalf("takeover after the crash: %v, %v", err, l)
		}
		l.Release()
	})

	// Hosts sharing only the store. A holder on host-b that crashes
	// mid-start blocks host-a until its record expires; a holder that
	// stalls past expiry has its late commit refused by the epoch.
	for _, role := range []string{"hold", "stall"} {
		t.Run("two hosts "+role, func(t *testing.T) {
			h := newLeaseHarness(t)
			onHost(t, "host-a")
			cityA := t.TempDir()
			other := start(t, h, role, "host-b", t.TempDir(), 2*time.Second)
			if _, err := h.try(cityA); !errors.Is(err, ErrRuntimeLeaseBusy) || strings.Contains(err.Error(), "on this host") {
				t.Fatalf("host-a while host-b holds = %v, want busy on host-b's record", err)
			}
			if role == "hold" {
				other.crash(t)
				if _, err := h.try(cityA); !errors.Is(err, ErrRuntimeLeaseBusy) {
					t.Fatalf("host-a right after host-b's crash = %v, want busy until expiry", err)
				}
			}
			l, err := WaitRuntimeLease(t.Context(), h.f.front, RuntimeLeaseRequest{City: cityA, Name: "s-lease", ID: h.f.id, TTL: leaseTTL}, 10*time.Second)
			if err != nil || l.Epoch() != 2 {
				t.Fatalf("host-a after expiry: %v, %v", err, l)
			}
			defer l.Release()
			if role == "stall" {
				other.proceed(t)
				other.expect(t, "commit lost=true")
				if m := h.f.meta(t); m["state"] == "stale-commit" || m[RuntimeLeaseEpochKey] != "2" || m[RuntimeLeaseHolderKey] != l.holder {
					t.Fatalf("row after the stale commit = %v, want host-a's lease and no commit", m)
				}
			}
		})
	}
}
