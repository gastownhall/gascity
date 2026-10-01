package scripts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

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
		// The launcher refuses actions while this chain is missing: it must name
		// the table and chain the nft ruleset below creates.
		"EGRESS_CHAIN": `"inet rbe_action output"`,
		// The image's world-writable directories, read-only inside actions.
		"RO_DIRS": `"$ro_dirs"`,
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

// slotEgressRules returns the nft ruleset blacksmith-worker.sh loads, one
// trimmed line per element.
func slotEgressRules(t *testing.T, script string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)sudo nft -f - <<-EOF\n(.*?)\n\tEOF\n`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s: no nft ruleset", rbeWorkerScript)
	}
	var lines []string
	for _, l := range strings.Split(m[1], "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	return lines
}

// The refused IPv4 classes, as on the MAIN worker (infra nftables-worker.conf):
// private, CGNAT/tailnet, link-local (cloud metadata), 0/8 and
// multicast/reserved. IPv6 is refused whole beyond loopback, ff00::/8 included.
const rbeSlotRefusedV4 = "{ 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 }"

func TestRBEWorkerScriptSlotEgress(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	rules := strings.Join(slotEgressRules(t, script), "\n")
	for _, want := range []string{
		"table inet rbe_action {",
		"chain output {",
		"type filter hook output priority 0; policy accept;",
		"meta skuid 59000-59063 meta nfproto ipv6 meta l4proto tcp reject with tcp reset",
		"meta skuid 59000-59063 meta nfproto ipv6 reject with icmpx admin-prohibited",
		"meta skuid 59000-59063 ip daddr " + rbeSlotRefusedV4 + " meta l4proto tcp reject with tcp reset",
		"meta skuid 59000-59063 ip daddr " + rbeSlotRefusedV4 + " reject with icmpx admin-prohibited",
		"update @slot_dst",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("slot egress rules missing %q", want)
		}
	}
	// A non-loopback IPv4 resolver is opened to slots on port 53 only; no
	// resolver slots can reach stops the script.
	for _, want := range []string{
		`dns_allow="meta skuid 59000-59063 ip daddr { $dns_v4 } meta l4proto { tcp, udp } th dport 53 accept"`,
		`echo "isolation: no nameserver in /etc/resolv.conf that slot users can reach (loopback or IPv4): $nameservers" >&2`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s missing %q", rbeWorkerScript, want)
		}
	}
}

// Rule order is policy: nft takes the first verdict. Loopback (slots' own
// servers, the local resolver) and the resolver allowance come before every
// reject, and the inventory comes after them, so it only sees allowed traffic.
func TestRBEWorkerScriptSlotEgressRuleOrder(t *testing.T) {
	rules := slotEgressRules(t, readFile(t, repoRoot(t), rbeWorkerScript))
	order := []string{
		"type filter hook output priority 0; policy accept;",
		"oif lo accept",
		"$dns_allow",
		"meta skuid 59000-59063 meta nfproto ipv6 meta l4proto tcp reject with tcp reset",
		"meta skuid 59000-59063 meta nfproto ipv6 reject with icmpx admin-prohibited",
		"meta skuid 59000-59063 ip daddr " + rbeSlotRefusedV4 + " meta l4proto tcp reject with tcp reset",
		"meta skuid 59000-59063 ip daddr " + rbeSlotRefusedV4 + " reject with icmpx admin-prohibited",
		"meta skuid 59000-59063 meta l4proto { tcp, udp } ct state new update @slot_dst { ip daddr . meta l4proto . th dport }",
	}
	at := 0
	for _, want := range order {
		i := at
		for i < len(rules) && rules[i] != want {
			i++
		}
		if i == len(rules) {
			t.Fatalf("slot egress rule %q missing or out of order in:\n%s", want, strings.Join(rules, "\n"))
		}
		at = i + 1
	}
	// Nothing else in the output chain: no accept or reject slipped in between.
	var verdicts []string
	for _, l := range rules {
		if strings.Contains(l, "accept") || strings.Contains(l, "reject") || l == "$dns_allow" {
			verdicts = append(verdicts, l)
		}
	}
	if got, want := len(verdicts), 7; got != want {
		t.Errorf("output chain has %d verdict rules, want %d:\n%s", got, want, strings.Join(verdicts, "\n"))
	}
}

// With isolation off (the RBE_ACTION_ISOLATION=0 rollback) the script must
// render exactly the worker.json it rendered before O1. The golden file is
// origin/main's jq program before O1 (4d0e45d9eb^) rendered with the same
// arguments; regenerate it only for an intended worker config change.
func TestRBEWorkerJSONIsolationOffMatchesPreO1(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerScript)
	m := regexp.MustCompile(`(?s)--argjson isolation "\$isolation" '\n(.*?)' >"\$ROOT/worker.json"\n`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s: no worker.json jq program", rbeWorkerScript)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "jq", "-n",
		"--arg", "host", "grpcs://rbe-west.example.invalid:443",
		"--arg", "root", "/home/runner/work/_temp/nl-worker",
		"--arg", "store", "/home/runner/work/_temp/nl-worker",
		"--arg", "name", "pool-worker-1",
		"--argjson", "slots", "8",
		"--argjson", "isolation", "{}",
		m[1])
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq (needed by %s itself): %v\n%s", rbeWorkerScript, err, stderr.String())
	}
	golden, err := os.ReadFile(filepath.Join(root, "scripts", "testdata", "rbe-worker", "worker-isolation-off.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	decode := func(b []byte) any {
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatalf("decode: %v\n%s", err, b)
		}
		return v
	}
	if got, want := decode(out), decode(golden); !reflect.DeepEqual(got, want) {
		t.Errorf("worker.json with isolation='{}' differs from the pre-O1 rendering:\ngot:\n%s\nwant:\n%s", out, golden)
	}
}

func TestRBEWorkerScriptGatesNativeLinkOnIsolation(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerScript)
	// Install, prove, then start NativeLink; never the other way round.
	order := []string{
		// Slot uids/gids are free before any slot user is created.
		`taken=$(awk -F: '$3 >= 59000 && $3 <= 59063 { print FILENAME ": " $1 " (" $3 ")" }' /etc/passwd /etc/group)`,
		`[ -z "$taken" ] || { echo "isolation: uids/gids 59000-59063 must be free for the slot users, taken: $taken" >&2; exit 1; }`,
		`sudo groupadd --system --gid "$id" "$u"`,
		`gcc -static -O2 -Wall -Wextra -o "$RUNNER_TEMP/rbe-entry" tools/rbe/rbe-action-entry.c`,
		`gcc -static -O2 -Wall -Wextra -DRBE_ACTION_EXEC -o "$RUNNER_TEMP/rbe-exec" tools/rbe/rbe-action-entry.c`,
		`sudo install -m 0755 tools/rbe/rbe-action-launch "$LIB/launch"`,
		`sudo install -m 0755 tools/rbe/rbe-action-sweep "$LIB/sweep"`,
		`sudo install -m 0755 tools/rbe/rbe-action-selftest "$LIB/selftest"`,
		// RO_DIRS is discovered before rbe-action.env is written.
		`found=$(sudo timeout 300 find / -xdev -path "$MASK_ROOT" -prune -o -type d -perm -0002 -print 2>/dev/null) || rc=$?`,
		`sudo tee /etc/rbe-west/rbe-action.env >/dev/null <<-EOF`,
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

// Run 36861390718 died without a word right after the script took world write
// off the image's shared directories: the runner (or Blacksmith's agent)
// relies on them. Isolation leaves the host's permissions alone (the launcher
// makes those directories read-only inside each action, RO_DIRS), and logs
// every phase so a silent death still shows where it happened.
func TestRBEWorkerScriptLeavesHostPermissionsAlone(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	for _, bad := range []string{"chmod o-w", "chmod -R", "xargs -r -d '\\n' sudo chmod"} {
		if strings.Contains(script, bad) {
			t.Errorf("%s must not change the image's permissions (%q); RO_DIRS makes them read-only for actions", rbeWorkerScript, bad)
		}
	}
	at := 0
	for _, phase := range []string{"packages", "users", "compile", "install", "ro-dirs", "sudoers", "nft", "render", "selftest", "probe"} {
		want := "echo \"isolation: " + phase + "\"\n"
		i := strings.Index(script[at:], want)
		if i < 0 {
			t.Fatalf("%s: phase marker %q missing or out of order", rbeWorkerScript, want)
		}
		at += i + len(want)
	}
}
