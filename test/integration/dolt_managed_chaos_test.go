//go:build integration && chaos_dolt

package integration

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	defaultManagedDoltChaosDuration = 2 * time.Minute
	minManagedDoltChaosDuration     = 5 * time.Second
	managedDoltChaosMaxLedger       = 24
	managedDoltRecoveryTimeout      = 20 * time.Second
	managedDoltPIDExitTimeout       = 10 * time.Second
)

type managedDoltChaosScope string

const (
	managedDoltChaosCityScope managedDoltChaosScope = "city"
	managedDoltChaosRigScope  managedDoltChaosScope = "rig"
)

type managedDoltChaosEntry struct {
	ID    string
	Title string
	Scope managedDoltChaosScope
}

type managedDoltChaosMail struct {
	Recipient string
	Body      string
}

type managedDoltChaosListItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type managedDoltChaosRuntimeState struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	DataDir   string `json:"data_dir"`
	StartedAt string `json:"started_at"`
}

type managedDoltChaosHarness struct {
	t          *testing.T
	cityDir    string
	rigDir     string
	rigName    string
	rng        *rand.Rand
	ledger     []managedDoltChaosEntry
	mailLedger []managedDoltChaosMail
	createSeq  int
	mailSeq    int
	hardKills  int
	rebinds    int
}

func TestManagedDoltChaos_CityAndRigCallersRemainConsistent(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}

	ensureDoltIdentity(t)

	duration := managedDoltChaosDurationFromEnv(t)
	seed := managedDoltChaosSeedFromEnv(t)
	deadline := time.Now().Add(duration)
	t.Logf("managed dolt chaos seed=%d duration=%s", seed, duration)

	h := setupManagedDoltChaosHarness(t, seed)
	if err := h.prime(); err != nil {
		t.Fatalf("prime managed dolt chaos harness: %v", err)
	}
	if !time.Now().Before(deadline) {
		t.Fatalf("managed dolt chaos budget %s exhausted during setup/prime", duration)
	}

	nextFault := time.Now().Add(h.nextFaultInterval())
	for time.Now().Before(deadline) {
		var (
			op  string
			err error
		)
		if time.Now().After(nextFault) {
			forceRebind := h.hardKills%2 == 0
			op = "fault"
			err = h.injectFault(forceRebind)
			nextFault = time.Now().Add(h.nextFaultInterval())
		} else {
			op, err = h.runRandomOperation()
		}
		t.Logf("managed dolt chaos op=%s", op)
		t.Logf("managed dolt chaos state %s", h.debugStateSummary())
		if err != nil {
			t.Fatalf("%s failed: %v", op, err)
		}
		if err := h.assertInvariants(); err != nil {
			t.Fatalf("%s invariant failure: %v", op, err)
		}
	}

	if h.hardKills == 0 {
		t.Fatal("chaos run finished without any managed Dolt hard-kill faults")
	}
	if h.rebinds == 0 {
		t.Fatal("chaos run finished without any forced managed Dolt port rebinds")
	}
	if err := h.assertFullLedgerVisibility(); err != nil {
		t.Fatalf("final ledger visibility failure: %v", err)
	}
}

func TestManagedDoltMailRebindRawBDReady(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}

	ensureDoltIdentity(t)

	h := setupManagedDoltChaosHarness(t, 1)
	if err := h.prime(); err != nil {
		t.Fatalf("prime managed dolt chaos harness: %v", err)
	}

	before, err := h.waitForManagedRuntimeState(managedDoltRecoveryTimeout, func(state managedDoltChaosRuntimeState) bool {
		return state.Running && state.PID > 0 && state.Port > 0
	})
	if err != nil {
		t.Fatalf("read managed runtime before fault: %v", err)
	}
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill managed pid %d: %v", before.PID, err)
	}
	if err := waitForManagedDoltPIDExit(before.PID, managedDoltPIDExitTimeout); err != nil {
		t.Fatalf("wait for managed pid %d exit: %v", before.PID, err)
	}

	releasePort, err := occupyManagedDoltPort(before.Port, 5*time.Second)
	if err != nil {
		t.Fatalf("occupy old managed port %d: %v", before.Port, err)
	}
	defer func() { _ = releasePort() }()

	out, err := gcDolt(h.cityDir, "mail", "send", "rig-worker", "managed-dolt-mail-rebind")
	if err != nil {
		t.Fatalf("gc mail send rig-worker after hard kill: %v\n%s", err, out)
	}

	cityRaw, cityErr := h.runCityRawBD("list", "--json", "--all", "--limit=0")
	rigRaw, rigErr := h.runRigRawBD("list", "--json", "--all", "--limit=0")
	after, afterErr := h.waitForManagedRuntimeState(managedDoltRecoveryTimeout, func(state managedDoltChaosRuntimeState) bool {
		if !state.Running || state.PID <= 0 || state.Port <= 0 {
			return false
		}
		if state.PID == before.PID {
			return false
		}
		return state.Port != before.Port
	})
	if cityErr != nil || rigErr != nil {
		if afterErr == nil {
			_ = h.waitForPortMirrors(after.Port, 5*time.Second)
		}
		t.Fatalf("raw bd not ready when gc mail send returned; cityErr=%v cityOut=%s rigErr=%v rigOut=%s after=%+v afterErr=%v", cityErr, cityRaw, rigErr, rigRaw, after, afterErr)
	}
	if afterErr != nil {
		t.Fatalf("managed runtime did not publish a replacement pid/port after raw bd recovered; afterErr=%v cityOut=%s rigOut=%s", afterErr, cityRaw, rigRaw)
	}
	if err := h.waitForPortMirrors(after.Port, 5*time.Second); err != nil {
		t.Fatalf("managed runtime rebind did not update port mirrors to %d: %v", after.Port, err)
	}
}

func TestManagedDoltMailInboxCityRecoveryKeepsScopesRawReady(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}

	ensureDoltIdentity(t)

	h := setupManagedDoltChaosHarness(t, 3)
	if err := h.prime(); err != nil {
		t.Fatalf("prime managed dolt chaos harness: %v", err)
	}

	cityEntry, ok := latestManagedDoltChaosEntry(h.ledger, managedDoltChaosCityScope)
	if !ok {
		t.Fatal("missing city bead after prime")
	}
	rigEntry, ok := latestManagedDoltChaosEntry(h.ledger, managedDoltChaosRigScope)
	if !ok {
		t.Fatal("missing rig bead after prime")
	}

	before, err := h.waitForManagedRuntimeState(managedDoltRecoveryTimeout, func(state managedDoltChaosRuntimeState) bool {
		return state.Running && state.PID > 0 && state.Port > 0
	})
	if err != nil {
		t.Fatalf("read managed runtime before fault: %v", err)
	}
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill managed pid %d: %v", before.PID, err)
	}
	if err := waitForManagedDoltPIDExit(before.PID, managedDoltPIDExitTimeout); err != nil {
		t.Fatalf("wait for managed pid %d exit: %v", before.PID, err)
	}

	releasePort, err := occupyManagedDoltPort(before.Port, 5*time.Second)
	if err != nil {
		t.Fatalf("occupy old managed port %d: %v", before.Port, err)
	}
	defer func() { _ = releasePort() }()

	out, err := gcDolt(h.cityDir, "mail", "inbox", "city-worker")
	if err != nil {
		t.Fatalf("gc mail inbox city-worker after hard kill old_port=%d: %v\n%s", before.Port, err, out)
	}

	after, afterErr := h.waitForManagedRuntimeState(managedDoltRecoveryTimeout, func(state managedDoltChaosRuntimeState) bool {
		if !state.Running || state.PID <= 0 || state.Port <= 0 {
			return false
		}
		return state.PID != before.PID
	})
	if afterErr != nil {
		t.Fatalf("managed runtime did not publish a replacement pid after city inbox recovery: %v", afterErr)
	}
	if err := h.waitForPortMirrors(after.Port, 5*time.Second); err != nil {
		t.Fatalf("managed runtime did not update port mirrors to %d after city inbox recovery: %v", after.Port, err)
	}

	rigShow, rigShowErr := h.runRigRawBD("show", rigEntry.ID, "--json")
	if rigShowErr != nil {
		t.Fatalf("rig raw show %s after city inbox recovery: %v\n%s", rigEntry.ID, rigShowErr, rigShow)
	}
	cityShow, cityShowErr := h.runCityGCBD("show", cityEntry.ID, "--json")
	if cityShowErr != nil {
		t.Fatalf("city gc show %s after city inbox recovery: %v\n%s", cityEntry.ID, cityShowErr, cityShow)
	}

	cityRaw, cityErr := h.runCityRawBD("list", "--json", "--all", "--limit=0")
	rigRaw, rigErr := h.runRigRawBD("list", "--json", "--all", "--limit=0")
	if cityErr != nil || rigErr != nil {
		t.Fatalf("raw bd not ready after city inbox recovery followup; cityErr=%v cityOut=%s rigErr=%v rigOut=%s after=%+v afterErr=%v state=%s", cityErr, cityRaw, rigErr, rigRaw, after, afterErr, h.debugStateSummary())
	}
}

func TestManagedDoltConcurrentRecoveryLeavesRawBDReady(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}

	ensureDoltIdentity(t)

	h := setupManagedDoltChaosHarness(t, 2)
	if err := h.prime(); err != nil {
		t.Fatalf("prime managed dolt chaos harness: %v", err)
	}

	before, err := h.waitForManagedRuntimeState(managedDoltRecoveryTimeout, func(state managedDoltChaosRuntimeState) bool {
		return state.Running && state.PID > 0 && state.Port > 0
	})
	if err != nil {
		t.Fatalf("read managed runtime before fault: %v", err)
	}
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill managed pid %d: %v", before.PID, err)
	}
	if err := waitForManagedDoltPIDExit(before.PID, managedDoltPIDExitTimeout); err != nil {
		t.Fatalf("wait for managed pid %d exit: %v", before.PID, err)
	}

	releasePort, err := occupyManagedDoltPort(before.Port, 5*time.Second)
	if err != nil {
		t.Fatalf("occupy old managed port %d: %v", before.Port, err)
	}
	defer func() { _ = releasePort() }()

	type opResult struct {
		name string
		out  string
		err  error
	}
	results := make(chan opResult, 2)
	go func() {
		out, err := gcDolt(h.cityDir, "bd", "list", "--json", "--all", "--limit=0")
		results <- opResult{name: "gc bd list", out: out, err: err}
	}()
	go func() {
		out, err := gcDolt(h.cityDir, "mail", "send", "rig-worker", "managed-dolt-mail-concurrent-rebind")
		results <- opResult{name: "gc mail send rig-worker", out: out, err: err}
	}()

	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s after hard kill: %v\n%s", result.name, result.err, result.out)
		}
	}

	after, afterErr := h.waitForManagedRuntimeState(managedDoltRecoveryTimeout, func(state managedDoltChaosRuntimeState) bool {
		if !state.Running || state.PID <= 0 || state.Port <= 0 {
			return false
		}
		if state.PID == before.PID {
			return false
		}
		return state.Port != before.Port
	})
	if afterErr != nil {
		t.Fatalf("managed runtime did not publish a replacement pid/port after concurrent recovery: %v", afterErr)
	}
	if err := h.waitForPortMirrors(after.Port, 5*time.Second); err != nil {
		t.Fatalf("managed runtime did not update port mirrors to %d after concurrent recovery: %v", after.Port, err)
	}

	cityRaw, cityErr := h.runCityRawBD("list", "--json", "--all", "--limit=0")
	rigRaw, rigErr := h.runRigRawBD("list", "--json", "--all", "--limit=0")
	if cityErr != nil || rigErr != nil {
		t.Fatalf("raw bd not ready after concurrent recovery; cityErr=%v cityOut=%s rigErr=%v rigOut=%s after=%+v", cityErr, cityRaw, rigErr, rigRaw, after)
	}
}

func setupManagedDoltChaosHarness(t *testing.T, seed int64) *managedDoltChaosHarness {
	t.Helper()

	env := newIsolatedCommandEnv(t, true)
	root, err := os.MkdirTemp("/tmp", "mdc-*")
	if err != nil {
		t.Fatalf("mktemp short chaos root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cityDir := filepath.Join(root, "c")
	rigDir := filepath.Join(root, "fe")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}

	configPath := filepath.Join(root, "managed-dolt-chaos.toml")
	config := fmt.Sprintf(`[workspace]
name = "managed-dolt-chaos"

[beads]
provider = "bd"

[session]
provider = "subprocess"

[daemon]
patrol_interval = "100ms"

[[rigs]]
name = "frontend"
path = %q
prefix = "fe"

[[agent]]
name = "city-worker"
start_command = "sleep 3600"

[[agent]]
name = "rig-worker"
start_command = "sleep 3600"
dir = "frontend"

[[named_session]]
template = "city-worker"
mode = "always"

[[named_session]]
template = "rig-worker"
mode = "always"
dir = "frontend"
`, rigDir)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("write chaos config: %v", err)
	}

	out, err := runGCDoltWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init chaos city: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)      //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop") //nolint:errcheck // best-effort cleanup
	})

	waitForActiveSessionTargets(t, cityDir, []string{"city-worker", "frontend/rig-worker"}, 30*time.Second)

	return &managedDoltChaosHarness{
		t:       t,
		cityDir: cityDir,
		rigDir:  rigDir,
		rigName: "frontend",
		rng:     rand.New(rand.NewSource(seed)),
	}
}

func (h *managedDoltChaosHarness) prime() error {
	cityRaw, err := h.listEntries("prime city raw", h.runCityRawBD)
	if err != nil {
		return err
	}
	cityGC, err := h.listEntries("prime city gc", h.runCityGCBD)
	if err != nil {
		return err
	}
	rigRaw, err := h.listEntries("prime rig raw", h.runRigRawBD)
	if err != nil {
		return err
	}
	rigGC, err := h.listEntries("prime rig gc", h.runRigGCBD)
	if err != nil {
		return err
	}
	if err := assertManagedDoltChaosExactList("prime city raw/gc", cityRaw, cityGC); err != nil {
		return err
	}
	if err := assertManagedDoltChaosExactList("prime rig raw/gc", rigRaw, rigGC); err != nil {
		return err
	}
	if err := assertManagedDoltChaosDisjointScopes(cityRaw, rigRaw); err != nil {
		return err
	}

	if _, _, err := h.createCityRaw(); err != nil {
		return fmt.Errorf("prime city raw create: %w", err)
	}
	if _, _, err := h.createRigGC(); err != nil {
		return fmt.Errorf("prime rig gc create: %w", err)
	}
	return h.assertInvariants()
}

func managedDoltChaosDurationFromEnv(t *testing.T) time.Duration {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv("GC_DOLT_CHAOS_DURATION"))
	if raw == "" {
		return defaultManagedDoltChaosDuration
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("parse GC_DOLT_CHAOS_DURATION=%q: %v", raw, err)
	}
	if d < minManagedDoltChaosDuration {
		return minManagedDoltChaosDuration
	}
	return d
}

func managedDoltChaosSeedFromEnv(t *testing.T) int64 {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv("GC_DOLT_CHAOS_SEED"))
	if raw == "" {
		return time.Now().UnixNano()
	}
	seed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("parse GC_DOLT_CHAOS_SEED=%q: %v", raw, err)
	}
	return seed
}

func (h *managedDoltChaosHarness) nextFaultInterval() time.Duration {
	return time.Duration(3+h.rng.Intn(5)) * time.Second
}

func (h *managedDoltChaosHarness) runRandomOperation() (string, error) {
	canCreate := len(h.ledger) < managedDoltChaosMaxLedger
	roll := h.rng.Intn(10)
	switch {
	case canCreate && roll == 0:
		_, title, err := h.createCityRaw()
		return "city raw create " + title, err
	case canCreate && roll == 1:
		_, title, err := h.createCityGC()
		return "city gc create " + title, err
	case canCreate && roll == 2:
		_, title, err := h.createRigRaw()
		return "rig raw create " + title, err
	case canCreate && roll == 3:
		_, title, err := h.createRigGC()
		return "rig gc create " + title, err
	case roll == 4:
		return h.randomShow()
	case roll == 5:
		return h.randomList()
	case roll == 6:
		return h.sendMail("city-worker")
	case roll == 7:
		return h.sendMail("rig-worker")
	default:
		return h.randomShow()
	}
}

func (h *managedDoltChaosHarness) createCityRaw() (string, string, error) {
	return h.createWithRunner("city raw", managedDoltChaosCityScope, h.runCityRawBD)
}

func (h *managedDoltChaosHarness) createCityGC() (string, string, error) {
	return h.createWithRunner("city gc", managedDoltChaosCityScope, h.runCityGCBD)
}

func (h *managedDoltChaosHarness) createRigRaw() (string, string, error) {
	return h.createWithRunner("rig raw", managedDoltChaosRigScope, h.runRigRawBD)
}

func (h *managedDoltChaosHarness) createRigGC() (string, string, error) {
	return h.createWithRunner("rig gc", managedDoltChaosRigScope, h.runRigGCBD)
}

func (h *managedDoltChaosHarness) createWithRunner(name string, scope managedDoltChaosScope, run func(...string) (string, error)) (string, string, error) {
	h.createSeq++
	title := fmt.Sprintf("managed-dolt-chaos-%s-%02d", scope, h.createSeq)
	before, err := h.listEntries(name+" pre-create", run)
	if err != nil {
		return "", title, err
	}
	out, err := run("create", "--json", title)
	if err != nil {
		return "", title, fmt.Errorf("%s create: %v\n%s", name, err, out)
	}
	after, err := h.listEntries(name+" post-create", run)
	if err != nil {
		return "", title, err
	}
	createdID, err := managedDoltChaosCreatedIDFromLists(before, after, title)
	if err != nil {
		return "", title, fmt.Errorf("%s create identify %q: %w\ncreate output:\n%s", name, title, err, out)
	}
	h.ledger = append(h.ledger, managedDoltChaosEntry{
		ID:    createdID,
		Title: title,
		Scope: scope,
	})
	return createdID, title, nil
}

func (h *managedDoltChaosHarness) randomShow() (string, error) {
	if len(h.ledger) == 0 {
		_, _, err := h.createCityRaw()
		return "city raw create bootstrap", err
	}
	entry := h.ledger[h.rng.Intn(len(h.ledger))]
	switch entry.Scope {
	case managedDoltChaosCityScope:
		if h.rng.Intn(2) == 0 {
			out, err := h.runCityRawBD("show", entry.ID, "--json")
			if err != nil {
				return "city raw show " + entry.ID, fmt.Errorf("city raw show: %v\n%s", err, out)
			}
			if err := assertManagedDoltChaosShow(out, entry); err != nil {
				return "city raw show " + entry.ID, err
			}
			return "city raw show " + entry.ID, nil
		}
		out, err := h.runCityGCBD("show", entry.ID, "--json")
		if err != nil {
			return "city gc show " + entry.ID, fmt.Errorf("city gc show: %v\n%s", err, out)
		}
		if err := assertManagedDoltChaosShow(out, entry); err != nil {
			return "city gc show " + entry.ID, err
		}
		return "city gc show " + entry.ID, nil
	default:
		if h.rng.Intn(2) == 0 {
			out, err := h.runRigRawBD("show", entry.ID, "--json")
			if err != nil {
				return "rig raw show " + entry.ID, fmt.Errorf("rig raw show: %v\n%s", err, out)
			}
			if err := assertManagedDoltChaosShow(out, entry); err != nil {
				return "rig raw show " + entry.ID, err
			}
			return "rig raw show " + entry.ID, nil
		}
		out, err := h.runRigGCBD("show", entry.ID, "--json")
		if err != nil {
			return "rig gc show " + entry.ID, fmt.Errorf("rig gc show: %v\n%s", err, out)
		}
		if err := assertManagedDoltChaosShow(out, entry); err != nil {
			return "rig gc show " + entry.ID, err
		}
		return "rig gc show " + entry.ID, nil
	}
}

func (h *managedDoltChaosHarness) randomList() (string, error) {
	switch h.rng.Intn(4) {
	case 0:
		out, err := h.runCityRawBD("list", "--json", "--all", "--limit=0")
		if err != nil {
			return "city raw list", fmt.Errorf("city raw list: %v\n%s", err, out)
		}
		if _, err := parseManagedDoltChaosList(out); err != nil {
			return "city raw list", err
		}
		return "city raw list", nil
	case 1:
		out, err := h.runCityGCBD("list", "--json", "--all", "--limit=0")
		if err != nil {
			return "city gc list", fmt.Errorf("city gc list: %v\n%s", err, out)
		}
		if _, err := parseManagedDoltChaosList(out); err != nil {
			return "city gc list", err
		}
		return "city gc list", nil
	case 2:
		out, err := h.runRigRawBD("list", "--json", "--all", "--limit=0")
		if err != nil {
			return "rig raw list", fmt.Errorf("rig raw list: %v\n%s", err, out)
		}
		if _, err := parseManagedDoltChaosList(out); err != nil {
			return "rig raw list", err
		}
		return "rig raw list", nil
	default:
		out, err := h.runRigGCBD("list", "--json", "--all", "--limit=0")
		if err != nil {
			return "rig gc list", fmt.Errorf("rig gc list: %v\n%s", err, out)
		}
		if _, err := parseManagedDoltChaosList(out); err != nil {
			return "rig gc list", err
		}
		return "rig gc list", nil
	}
}

func (h *managedDoltChaosHarness) sendMail(recipient string) (string, error) {
	h.mailSeq++
	body := fmt.Sprintf("managed-dolt-mail-%s-%02d", recipient, h.mailSeq)
	out, err := gcDolt(h.cityDir, "mail", "send", recipient, body)
	if err != nil {
		return "mail send " + recipient, fmt.Errorf("gc mail send %s: %v\n%s", recipient, err, out)
	}
	h.mailLedger = append(h.mailLedger, managedDoltChaosMail{Recipient: recipient, Body: body})
	return "mail send " + recipient, nil
}

func (h *managedDoltChaosHarness) injectFault(forceRebind bool) error {
	before, err := h.waitForManagedRuntimeState(managedDoltRecoveryTimeout, func(state managedDoltChaosRuntimeState) bool {
		return state.Running && state.PID > 0 && state.Port > 0
	})
	if err != nil {
		return fmt.Errorf("read managed runtime state before fault: %w", err)
	}

	h.t.Logf("managed dolt chaos fault kill pid=%d port=%d forceRebind=%t", before.PID, before.Port, forceRebind)
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill managed pid %d: %w", before.PID, err)
	}
	if err := waitForManagedDoltPIDExit(before.PID, managedDoltPIDExitTimeout); err != nil {
		return fmt.Errorf("wait for managed pid %d exit: %w", before.PID, err)
	}

	var (
		releasePort func() error
		portLabel   string
	)
	if forceRebind {
		releasePort, err = occupyManagedDoltPort(before.Port, 5*time.Second)
		if err != nil {
			return fmt.Errorf("occupy old managed port %d: %w", before.Port, err)
		}
		portLabel = fmt.Sprintf(" port=%d", before.Port)
	}

	triggerName, triggerOut, triggerErr := h.runRecoveryTrigger()
	h.t.Logf("managed dolt chaos fault trigger=%s", triggerName)
	if triggerErr != nil {
		if releasePort != nil {
			_ = releasePort()
		}
		return fmt.Errorf("%s after hard kill%s: %v\n%s", triggerName, portLabel, triggerErr, triggerOut)
	}

	after, err := h.waitForManagedRuntimeState(managedDoltRecoveryTimeout, func(state managedDoltChaosRuntimeState) bool {
		if !state.Running || state.PID <= 0 || state.Port <= 0 {
			return false
		}
		if state.PID == before.PID {
			return false
		}
		if forceRebind && state.Port == before.Port {
			return false
		}
		return true
	})
	if releasePort != nil {
		if closeErr := releasePort(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return fmt.Errorf("wait for managed recovery via %s: %w", triggerName, err)
	}
	h.t.Logf("managed dolt chaos fault recovered pid=%d port=%d", after.PID, after.Port)
	if err := h.waitForPortMirrors(after.Port, 5*time.Second); err != nil {
		return fmt.Errorf("wait for managed port mirrors %d: %w", after.Port, err)
	}

	h.hardKills++
	if forceRebind {
		h.rebinds++
	}
	return nil
}

func (h *managedDoltChaosHarness) runRecoveryTrigger() (string, string, error) {
	// Only GC-owned entrypoints are allowed to trigger managed-Dolt recovery.
	// Raw bd should work again after recovery, but it is not the lifecycle owner.
	switch h.rng.Intn(4) {
	case 0:
		out, err := h.runCityGCBD("list", "--json", "--all", "--limit=0")
		return "city gc bd list", out, err
	case 1:
		out, err := h.runRigGCBD("list", "--json", "--all", "--limit=0")
		return "rig gc bd list", out, err
	case 2:
		out, err := gcDolt(h.cityDir, "mail", "inbox", "city-worker")
		return "gc mail inbox city-worker", out, err
	default:
		out, err := gcDolt(h.cityDir, "mail", "inbox", "rig-worker")
		return "gc mail inbox rig-worker", out, err
	}
}

func (h *managedDoltChaosHarness) assertInvariants() error {
	cityRaw, err := h.listEntries("city raw", h.runCityRawBD)
	if err != nil {
		return err
	}
	cityGC, err := h.listEntries("city gc", h.runCityGCBD)
	if err != nil {
		return err
	}
	rigRaw, err := h.listEntries("rig raw", h.runRigRawBD)
	if err != nil {
		return err
	}
	rigGC, err := h.listEntries("rig gc", h.runRigGCBD)
	if err != nil {
		return err
	}

	if err := assertManagedDoltChaosExactList("city raw/gc", cityRaw, cityGC); err != nil {
		return err
	}
	if err := assertManagedDoltChaosExactList("rig raw/gc", rigRaw, rigGC); err != nil {
		return err
	}
	if err := assertManagedDoltChaosDisjointScopes(cityRaw, rigRaw); err != nil {
		return err
	}
	for _, entry := range h.ledger {
		switch entry.Scope {
		case managedDoltChaosCityScope:
			if got := cityRaw[entry.ID]; got != entry.Title {
				return fmt.Errorf("city raw list missing %s title=%q got=%q", entry.ID, entry.Title, got)
			}
			if got := cityGC[entry.ID]; got != entry.Title {
				return fmt.Errorf("city gc list missing %s title=%q got=%q", entry.ID, entry.Title, got)
			}
			if got := rigRaw[entry.ID]; got != "" {
				return fmt.Errorf("rig raw list unexpectedly contains city bead %s title=%q", entry.ID, got)
			}
			if got := rigGC[entry.ID]; got != "" {
				return fmt.Errorf("rig gc list unexpectedly contains city bead %s title=%q", entry.ID, got)
			}
		case managedDoltChaosRigScope:
			if got := rigRaw[entry.ID]; got != entry.Title {
				return fmt.Errorf("rig raw list missing %s title=%q got=%q", entry.ID, entry.Title, got)
			}
			if got := rigGC[entry.ID]; got != entry.Title {
				return fmt.Errorf("rig gc list missing %s title=%q got=%q", entry.ID, entry.Title, got)
			}
			if got := cityRaw[entry.ID]; got != "" {
				return fmt.Errorf("city raw list unexpectedly contains rig bead %s title=%q", entry.ID, got)
			}
			if got := cityGC[entry.ID]; got != "" {
				return fmt.Errorf("city gc list unexpectedly contains rig bead %s title=%q", entry.ID, got)
			}
		}
	}

	if entry, ok := latestManagedDoltChaosEntry(h.ledger, managedDoltChaosCityScope); ok {
		if err := h.assertShow("city raw show", entry, h.runCityRawBD); err != nil {
			return err
		}
		if err := h.assertShow("city gc show", entry, h.runCityGCBD); err != nil {
			return err
		}
	}
	if entry, ok := latestManagedDoltChaosEntry(h.ledger, managedDoltChaosRigScope); ok {
		if err := h.assertShow("rig raw show", entry, h.runRigRawBD); err != nil {
			return err
		}
		if err := h.assertShow("rig gc show", entry, h.runRigGCBD); err != nil {
			return err
		}
	}

	if err := h.assertMailLedger(); err != nil {
		return err
	}

	state, err := h.waitForManagedRuntimeState(5*time.Second, func(state managedDoltChaosRuntimeState) bool {
		return state.Running && state.PID > 0 && state.Port > 0
	})
	if err != nil {
		return err
	}
	return h.waitForPortMirrors(state.Port, 5*time.Second)
}

func (h *managedDoltChaosHarness) debugStateSummary() string {
	state, stateErr := h.readManagedRuntimeState()
	cityPort, cityErr := readManagedDoltChaosPortFile(filepath.Join(h.cityDir, ".beads", "dolt-server.port"))
	rigPort, rigErr := readManagedDoltChaosPortFile(filepath.Join(h.rigDir, ".beads", "dolt-server.port"))
	return fmt.Sprintf("runtime=(running=%t pid=%d port=%d err=%v) mirrors=(city=%q err=%v rig=%q err=%v)", state.Running, state.PID, state.Port, stateErr, cityPort, cityErr, rigPort, rigErr)
}

func latestManagedDoltChaosEntry(entries []managedDoltChaosEntry, scope managedDoltChaosScope) (managedDoltChaosEntry, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Scope == scope {
			return entries[i], true
		}
	}
	return managedDoltChaosEntry{}, false
}

func (h *managedDoltChaosHarness) assertFullLedgerVisibility() error {
	for _, entry := range h.ledger {
		switch entry.Scope {
		case managedDoltChaosCityScope:
			if err := h.assertShow("city raw show", entry, h.runCityRawBD); err != nil {
				return err
			}
			if err := h.assertShow("city gc show", entry, h.runCityGCBD); err != nil {
				return err
			}
		case managedDoltChaosRigScope:
			if err := h.assertShow("rig raw show", entry, h.runRigRawBD); err != nil {
				return err
			}
			if err := h.assertShow("rig gc show", entry, h.runRigGCBD); err != nil {
				return err
			}
		}
	}
	return nil
}

func assertManagedDoltChaosExactList(name string, got, want map[string]string) error {
	for id, wantTitle := range want {
		gotTitle, ok := got[id]
		if !ok {
			return fmt.Errorf("%s list missing %s title=%q", name, id, wantTitle)
		}
		if gotTitle != wantTitle {
			return fmt.Errorf("%s list title mismatch for %s want=%q got=%q", name, id, wantTitle, gotTitle)
		}
	}
	for id, gotTitle := range got {
		wantTitle, ok := want[id]
		if !ok {
			return fmt.Errorf("%s list unexpectedly contains %s title=%q", name, id, gotTitle)
		}
		if wantTitle != gotTitle {
			return fmt.Errorf("%s list title mismatch for %s want=%q got=%q", name, id, wantTitle, gotTitle)
		}
	}
	return nil
}

func assertManagedDoltChaosDisjointScopes(city, rig map[string]string) error {
	for id, cityTitle := range city {
		if rigTitle, ok := rig[id]; ok {
			return fmt.Errorf("city/rig lists share bead %s city=%q rig=%q", id, cityTitle, rigTitle)
		}
	}
	return nil
}

func (h *managedDoltChaosHarness) assertShow(name string, entry managedDoltChaosEntry, run func(...string) (string, error)) error {
	out, err := run("show", entry.ID, "--json")
	if err != nil {
		return fmt.Errorf("%s %s: %v\n%s", name, entry.ID, err, out)
	}
	if err := assertManagedDoltChaosShow(out, entry); err != nil {
		return fmt.Errorf("%s %s: %w", name, entry.ID, err)
	}
	return nil
}

func (h *managedDoltChaosHarness) assertMailLedger() error {
	if len(h.mailLedger) == 0 {
		return nil
	}
	for _, recipient := range []string{"city-worker", "rig-worker"} {
		out, err := gcDolt(h.cityDir, "mail", "inbox", recipient)
		if err != nil {
			return fmt.Errorf("gc mail inbox %s: %v\n%s", recipient, err, out)
		}
		for _, msg := range h.mailLedger {
			if msg.Recipient != recipient {
				continue
			}
			if !strings.Contains(out, msg.Body) {
				return fmt.Errorf("gc mail inbox %s missing %q\n%s", recipient, msg.Body, out)
			}
		}
	}
	return nil
}

func (h *managedDoltChaosHarness) listEntries(name string, run func(...string) (string, error)) (map[string]string, error) {
	out, err := run("list", "--json", "--all", "--limit=0")
	if err != nil {
		return nil, fmt.Errorf("%s list: %v\n%s", name, err, out)
	}
	return parseManagedDoltChaosList(out)
}

func (h *managedDoltChaosHarness) rawBDEnv(workDir string) []string {
	env := commandEnvForDir(h.cityDir, true)
	env = filterEnv(env, "GC_CITY")
	env = filterEnv(env, "GC_CITY_PATH")
	env = filterEnv(env, "GC_CITY_ROOT")
	env = filterEnv(env, "GC_CITY_RUNTIME_DIR")
	env = append(env,
		"GC_CITY="+h.cityDir,
		"GC_CITY_PATH="+h.cityDir,
		"GC_CITY_RUNTIME_DIR="+filepath.Join(h.cityDir, ".gc", "runtime"),
	)
	for _, portPath := range []string{
		filepath.Join(workDir, ".beads", "dolt-server.port"),
		filepath.Join(h.cityDir, ".beads", "dolt-server.port"),
	} {
		data, err := os.ReadFile(portPath)
		if err != nil {
			continue
		}
		port := strings.TrimSpace(string(data))
		if port == "" {
			continue
		}
		env = replaceEnv(env, "GC_DOLT_PORT", port)
		env = replaceEnv(env, "BEADS_DOLT_SERVER_PORT", port)
		break
	}
	return env
}

func (h *managedDoltChaosHarness) runCityRawBD(args ...string) (string, error) {
	return runCommand(h.cityDir, h.rawBDEnv(h.cityDir), integrationBDCommandTimeout, bdBinary, args...)
}

func (h *managedDoltChaosHarness) runRigRawBD(args ...string) (string, error) {
	return runCommand(h.rigDir, h.rawBDEnv(h.rigDir), integrationBDCommandTimeout, bdBinary, args...)
}

func (h *managedDoltChaosHarness) runCityGCBD(args ...string) (string, error) {
	return gcDolt(h.cityDir, append([]string{"bd"}, args...)...)
}

func (h *managedDoltChaosHarness) runRigGCBD(args ...string) (string, error) {
	gcArgs := append([]string{"bd", "--rig", h.rigName}, args...)
	return gcDolt(h.cityDir, gcArgs...)
}

func (h *managedDoltChaosHarness) waitForManagedRuntimeState(timeout time.Duration, ok func(managedDoltChaosRuntimeState) bool) (managedDoltChaosRuntimeState, error) {
	deadline := time.Now().Add(timeout)
	lastErr := "no managed runtime state observed"
	for time.Now().Before(deadline) {
		state, err := h.readManagedRuntimeState()
		if err == nil {
			if ok(state) {
				return state, nil
			}
			lastErr = fmt.Sprintf("state not ready: %+v", state)
		} else {
			lastErr = err.Error()
		}
		time.Sleep(150 * time.Millisecond)
	}
	return managedDoltChaosRuntimeState{}, fmt.Errorf("%s", lastErr)
}

func (h *managedDoltChaosHarness) readManagedRuntimeState() (managedDoltChaosRuntimeState, error) {
	data, err := os.ReadFile(filepath.Join(h.cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json"))
	if err != nil {
		return managedDoltChaosRuntimeState{}, err
	}
	var state managedDoltChaosRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return managedDoltChaosRuntimeState{}, err
	}
	return state, nil
}

func (h *managedDoltChaosHarness) waitForPortMirrors(port int, timeout time.Duration) error {
	want := strconv.Itoa(port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cityPort, cityErr := readManagedDoltChaosPortFile(filepath.Join(h.cityDir, ".beads", "dolt-server.port"))
		rigPort, rigErr := readManagedDoltChaosPortFile(filepath.Join(h.rigDir, ".beads", "dolt-server.port"))
		if cityErr == nil && rigErr == nil && cityPort == want && rigPort == want {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	cityPort, cityErr := readManagedDoltChaosPortFile(filepath.Join(h.cityDir, ".beads", "dolt-server.port"))
	rigPort, rigErr := readManagedDoltChaosPortFile(filepath.Join(h.rigDir, ".beads", "dolt-server.port"))
	return fmt.Errorf("port mirrors want=%s city=(%q,%v) rig=(%q,%v)", want, cityPort, cityErr, rigPort, rigErr)
}

func readManagedDoltChaosPortFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	port := strings.TrimSpace(string(data))
	if port == "" {
		return "", fmt.Errorf("%s empty", path)
	}
	return port, nil
}

func occupyManagedDoltPort(port int, timeout time.Duration) (func() error, error) {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln.Close, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out binding %s", addr)
}

func waitForManagedDoltPIDExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err != nil {
			if err == syscall.ESRCH {
				return nil
			}
			if err != syscall.EPERM {
				return err
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pid %d still alive after %s", pid, timeout)
}

func managedDoltChaosCreatedIDFromLists(before, after map[string]string, title string) (string, error) {
	matches := make([]string, 0, 1)
	for id, gotTitle := range after {
		if gotTitle != title {
			continue
		}
		if _, existed := before[id]; existed {
			continue
		}
		matches = append(matches, id)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no new bead with title %q appeared", title)
	default:
		return "", fmt.Errorf("multiple new beads with title %q appeared: %s", title, strings.Join(matches, ", "))
	}
}

func parseManagedDoltChaosCreated(raw string) (managedDoltChaosListItem, error) {
	data := []byte(strings.TrimSpace(extractJSONPayload(raw)))
	var item managedDoltChaosListItem
	if err := json.Unmarshal(data, &item); err == nil && item.ID != "" {
		return item, nil
	}
	var items []managedDoltChaosListItem
	if err := json.Unmarshal(data, &items); err != nil {
		return managedDoltChaosListItem{}, fmt.Errorf("parse create json: %w\n%s", err, raw)
	}
	if len(items) == 0 || items[0].ID == "" {
		return managedDoltChaosListItem{}, fmt.Errorf("create output missing bead id\n%s", raw)
	}
	return items[0], nil
}

func parseManagedDoltChaosList(raw string) (map[string]string, error) {
	data := []byte(strings.TrimSpace(extractJSONPayload(raw)))
	var items []managedDoltChaosListItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parse list json: %w\n%s", err, raw)
	}
	result := make(map[string]string, len(items))
	for _, item := range items {
		result[item.ID] = item.Title
	}
	return result, nil
}

func assertManagedDoltChaosShow(raw string, entry managedDoltChaosEntry) error {
	data := []byte(strings.TrimSpace(extractJSONPayload(raw)))
	var item managedDoltChaosListItem
	if err := json.Unmarshal(data, &item); err == nil && item.ID != "" {
		if item.ID != entry.ID || item.Title != entry.Title {
			return fmt.Errorf("show mismatch got=(%s,%q) want=(%s,%q)\n%s", item.ID, item.Title, entry.ID, entry.Title, raw)
		}
		return nil
	}
	var items []managedDoltChaosListItem
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("parse show json: %w\n%s", err, raw)
	}
	if len(items) == 0 {
		return fmt.Errorf("show returned no beads\n%s", raw)
	}
	if items[0].ID != entry.ID || items[0].Title != entry.Title {
		return fmt.Errorf("show mismatch got=(%s,%q) want=(%s,%q)\n%s", items[0].ID, items[0].Title, entry.ID, entry.Title, raw)
	}
	return nil
}
