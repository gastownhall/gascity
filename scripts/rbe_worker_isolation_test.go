package scripts_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The OSS remote-execution workers (tools/rbe/blacksmith-worker.sh, run by
// .github/workflows/rbe-worker-pool.yml on Blacksmith) execute actions from OSS
// CI and cherry agents' oss builds. Before S11.3 every action ran as the runner
// user: it could read pki/worker.key (the rbe-oss-worker cert, which writes the
// oss action cache and registers workers) and the step's environment, and sudo.
// These tests pin the action isolation that closes that (infra
// nativelink-cas/west README "Action isolation"; tools/rbe/rbe-action-* are
// copies of infra's) and the switch that rolls it back.

const (
	rbeWorkerScript   = "tools/rbe/blacksmith-worker.sh"
	rbeWorkerWorkflow = ".github/workflows/rbe-worker-pool.yml"
)

func TestRBEWorkerPoolWorkflowIsolatesActions(t *testing.T) {
	root := repoRoot(t)
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]any    `yaml:"with"`
				Env  map[string]string `yaml:"env"`
				Run  string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, rbeWorkerWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", rbeWorkerWorkflow, err)
	}
	job, ok := wf.Jobs["worker"]
	if !ok {
		t.Fatalf("%s: no worker job", rbeWorkerWorkflow)
	}
	var checkout, worker bool
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkout = true
			// Remote actions run on this runner: no GITHUB_TOKEN in .git/config.
			if v, _ := step.With["persist-credentials"].(bool); v || step.With["persist-credentials"] == nil {
				t.Errorf("checkout must set persist-credentials: false, got %v", step.With["persist-credentials"])
			}
		}
		if strings.Contains(step.Run, rbeWorkerScript) {
			worker = true
			// Default on; the repository variable RBE_ACTION_ISOLATION=0 is the
			// rollback without a code change.
			if got, want := step.Env["RBE_ACTION_ISOLATION"], "${{ vars.RBE_ACTION_ISOLATION || '1' }}"; got != want {
				t.Errorf("worker step RBE_ACTION_ISOLATION = %q, want %q", got, want)
			}
		}
	}
	if !checkout || !worker {
		t.Fatalf("%s: checkout step found %v, worker step found %v", rbeWorkerWorkflow, checkout, worker)
	}
}

func TestRBEWorkerScriptIsolationSwitch(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	for _, want := range []string{
		"ACTION_ISOLATION=${RBE_ACTION_ISOLATION:-1}",
		`*) echo "RBE_ACTION_ISOLATION must be 0 or 1" >&2; exit 2 ;;`,
		"isolation='{}'\nif [ \"$ACTION_ISOLATION\" = 1 ]; then\n",
		// The isolation keys are merged into the worker config only when on, so
		// the rollback renders today's worker.json.
		`} } + $isolation) } ],`,
		`--argjson isolation "$isolation"`,
		// Rollback starts NativeLink exactly as before.
		"else\n\t\"$NL_BIN_DIR/nativelink\" \"$ROOT/worker.json\" >\"$ROOT/worker.log\" 2>&1 &\nfi\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s missing %q", rbeWorkerScript, want)
		}
	}
}

func TestRBEWorkerScriptIsolationConfig(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)

	// NativeLink's half: the entrypoint and timeouts the launcher expects.
	m := regexp.MustCompile(`(?s)\n\tisolation='(\{.*?\})'\n`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s: no isolation='{...}' worker config", rbeWorkerScript)
	}
	var iso struct {
		Entrypoint               string            `json:"entrypoint"`
		TimeoutHandledExternally bool              `json:"timeout_handled_externally"`
		MaxActionTimeout         int               `json:"max_action_timeout"`
		AdditionalEnvironment    map[string]string `json:"additional_environment"`
	}
	if err := json.Unmarshal([]byte(m[1]), &iso); err != nil {
		t.Fatalf("isolation worker config is not JSON: %v\n%s", err, m[1])
	}
	if iso.Entrypoint != "/usr/local/libexec/rbe-action/entry" || !iso.TimeoutHandledExternally {
		t.Errorf("isolation worker config: entrypoint %q, timeout_handled_externally %v", iso.Entrypoint, iso.TimeoutHandledExternally)
	}
	if got := iso.AdditionalEnvironment; len(got) != 2 || got["RBE_X_TIMEOUT_MS"] != "timeout_millis" || got["RBE_X_SIDE_CHANNEL"] != "side_channel_file" {
		t.Errorf("additional_environment = %v, want RBE_X_TIMEOUT_MS and RBE_X_SIDE_CHANNEL only", got)
	}

	// The launcher's half (/etc/rbe-west/rbe-action.env).
	env := map[string]string{}
	block := regexp.MustCompile(`(?s)rbe-action\.env >/dev/null <<-EOF\n(.*?)\n\tEOF\n`).FindStringSubmatch(script)
	if block == nil {
		t.Fatalf("%s: no rbe-action.env heredoc", rbeWorkerScript)
	}
	for _, line := range strings.Split(block[1], "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		env[k] = v
	}
	for k, want := range map[string]string{
		"WORK_ROOT": "$WORK_ROOT", "MASK_ROOT": "$MASK_ROOT", "SLOT_UID0": "$SLOT_UID0", "SLOT_COUNT": "$slots",
		"BACKSTOP_S": "1260", "MAX_TIMEOUT_S": "1200", "HOME_DIR": "/var/lib/rbe-action/home",
		"NETNS": "0", "WORKER_JSON": "$ROOT/worker.json",
	} {
		if env[k] != want {
			t.Errorf("rbe-action.env %s = %q, want %q", k, env[k], want)
		}
	}
	// NativeLink kills the launcher at max_action_timeout, which must be the
	// launcher's backstop, beyond the longest action it allows.
	if iso.MaxActionTimeout != 1260 || env["BACKSTOP_S"] != "1260" {
		t.Errorf("max_action_timeout %d must equal BACKSTOP_S %s", iso.MaxActionTimeout, env["BACKSTOP_S"])
	}
	if !strings.Contains(script, "SLOT_UID0=59000\n") || !strings.Contains(script, `[ "$slots" -le 64 ] || slots=64`) {
		t.Error("slot uids must stay within 59000-59063, the range the nft rules cover")
	}
	// The worker key and the CAS must be under the mask the launcher mounts.
	if !strings.Contains(script, `for d in "$WORK_ROOT" "$(cd "$ROOT" && pwd -P)" "$(cd "$STORE" && pwd -P)"; do`) {
		t.Error("the script must refuse a ROOT, STORE or work directory outside MASK_ROOT")
	}
}

func TestRBEWorkerScriptSlotEgress(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	m := regexp.MustCompile(`(?s)sudo nft -f - <<-'EOF'\n(.*?)\n\tEOF\n`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s: no nft ruleset", rbeWorkerScript)
	}
	rules := m[1]
	for _, want := range []string{
		"type filter hook output priority 0; policy accept;",
		"meta skuid 59000-59063 meta nfproto ipv6 reject with icmpx admin-prohibited",
		"meta skuid 59000-59063 ip daddr { 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16 } reject with icmpx admin-prohibited",
		"update @slot_dst",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("slot egress rules missing %q", want)
		}
	}
}

func TestRBEWorkerScriptGatesNativeLinkOnIsolation(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerScript)
	// Install, prove, then start NativeLink; never the other way round.
	order := []string{
		`gcc -static -O2 -Wall -Wextra -o "$RUNNER_TEMP/rbe-entry" tools/rbe/rbe-action-entry.c`,
		`gcc -static -O2 -Wall -Wextra -DRBE_ACTION_EXEC -o "$RUNNER_TEMP/rbe-exec" tools/rbe/rbe-action-entry.c`,
		`sudo install -m 0755 tools/rbe/rbe-action-launch "$LIB/launch"`,
		`sudo install -m 0755 tools/rbe/rbe-action-sweep "$LIB/sweep"`,
		`sudo install -m 0755 tools/rbe/rbe-action-selftest "$LIB/selftest"`,
		`sudo chmod 0440 /etc/sudoers.d/rbe-action && sudo visudo -cq`,
		`sudo "$LIB/selftest"`,
		`LC_ALL=C sudo -l -U rbe-a00 2>&1 | grep -q 'not allowed to run sudo'`,
		`open_socks=$(sudo find /run /var/run -xdev -maxdepth 3 -type s -perm -o+w`,
		`probe "$ROOT/pki/worker.key"`,
		`if ! grep -qE "^uid 590[0-9]{2}$" <<<"$out" || grep -q LEAK <<<"$out"; then`,
		// NativeLink gets none of the step's environment (secrets included).
		`env -i PATH="$PATH" HOME="$HOME" "$NL_BIN_DIR/nativelink" "$ROOT/worker.json"`,
		"nl=$!",
	}
	at := 0
	for _, want := range order {
		i := strings.Index(script[at:], want)
		if i < 0 {
			t.Fatalf("%s: %q missing or out of order", rbeWorkerScript, want)
		}
		at += i + len(want)
	}
	// Killed launchers leave slot-owned directories that pool mode would count
	// as in flight: both poll loops sweep.
	if n := strings.Count(script, "while kill -0 \"$nl\" 2>/dev/null; do\n\t\tsweep\n"); n != 2 {
		t.Errorf("both poll loops must run the sweep first, found %d", n)
	}
	for _, f := range []string{"rbe-action-entry.c", "rbe-action-launch", "rbe-action-sweep", "rbe-action-selftest"} {
		body := readFile(t, root, "tools/rbe/"+f)
		if !strings.Contains(body, f+": ") || !strings.Contains(body, "/usr/local/libexec/rbe-action/") {
			t.Errorf("tools/rbe/%s is not infra's rbe-action file", f)
		}
	}
}
