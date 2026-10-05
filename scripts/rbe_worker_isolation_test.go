package scripts_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
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
		// The worker-env measure step runs the script too, without a worker.
		if strings.Contains(step.Run, rbeWorkerScript) && step.Env["WORKER_MODE"] != "measure" {
			worker = true
			// Default on; the repository variable RBE_ACTION_ISOLATION=0 is the
			// rollback without a code change.
			if got, want := step.Env["RBE_ACTION_ISOLATION"], "${{ vars.RBE_ACTION_ISOLATION || '1' }}"; got != want {
				t.Errorf("worker step RBE_ACTION_ISOLATION = %q, want %q", got, want)
			}
			// canary: one run in RBE_ACTION_CANARY_EVERY tries isolation.
			if got, want := step.Env["RBE_ACTION_CANARY_EVERY"], "${{ vars.RBE_ACTION_CANARY_EVERY || '4' }}"; got != want {
				t.Errorf("worker step RBE_ACTION_CANARY_EVERY = %q, want %q", got, want)
			}
			// zstd fetches, off unless the repository variable says 1: merging
			// changes nothing, and rollback is the variable.
			if got, want := step.Env["RBE_WIRE_ZSTD"], "${{ vars.RBE_WIRE_ZSTD || '0' }}"; got != want {
				t.Errorf("worker step RBE_WIRE_ZSTD = %q, want %q", got, want)
			}
			// The dedicated zread host; the worker refuses zstd without it.
			if got, want := step.Env["RBE_WIRE_ZSTD_READ_URL"], "${{ vars.RBE_WIRE_ZSTD_READ_URL || '' }}"; got != want {
				t.Errorf("worker step RBE_WIRE_ZSTD_READ_URL = %q, want %q", got, want)
			}
			// The OSS pool keeps the script's defaults (tier oss, :443) and its
			// own certificate; the fork tier is rbe-fork-pool.yml's alone.
			for _, k := range []string{"WORKER_TIER", "RBE_WEST_PORT"} {
				if v, ok := step.Env[k]; ok {
					t.Errorf("worker step sets %s=%q; the OSS pool runs the defaults", k, v)
				}
			}
			if got, want := step.Env["RBE_WORKER_TLS_KEY"], "${{ secrets.RBE_WORKER_TLS_KEY }}"; got != want {
				t.Errorf("worker step RBE_WORKER_TLS_KEY = %q, want %q", got, want)
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
		"0 | 1 | canary) ;;\n",
		`*) echo "RBE_ACTION_ISOLATION must be 0, 1 or canary" >&2; exit 2 ;;`,
		"plain_slots=$slots\nisolation='{}'\nif [ \"$ACTION_ISOLATION\" = 1 ]; then\n",
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
		// Set by WORKER_TIER alone (TestRBEWorkerScriptForkTier): 0 for oss,
		// 1 (loopback only) for fork.
		"NETNS": "${NETNS:-0}", "WORKER_JSON": "$ROOT/worker.json",
		// Never infra's privileged network namespace class (RBE_X_NETNS_ROOT=1,
		// MAIN only): a no-op for this launcher copy until it is synced, then
		// the pin.
		"NETNS_ROOT": "0",
		// The launcher refuses actions while this chain is missing: it must name
		// the table and chain the nft ruleset below creates.
		"EGRESS_CHAIN": `"inet rbe_action output"`,
		// / and every other mount but the action's own are read-only inside
		// actions, whatever the image leaves world-writable.
		"ROOT_RO": "1",
		// connect() works on a read-only mount: world-writable sockets on
		// /run (Blacksmith's VM shutdown socket, snapd, ...) are masked inside
		// actions (run 36956951091 found eight).
		"MASK_SOCKETS": "1",
	} {
		if env[k] != want {
			t.Errorf("rbe-action.env %s = %q, want %q", k, env[k], want)
		}
	}
	// The launcher refuses RO_DIRS (ROOT_RO=1 replaced it).
	if _, ok := env["RO_DIRS"]; ok {
		t.Errorf("rbe-action.env must not set RO_DIRS (the launcher refuses it): %q", env["RO_DIRS"])
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
		`fail "no nameserver in /etc/resolv.conf that slot users can reach (loopback or IPv4): $nameservers"`,
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
// arguments; regenerate it only for an intended worker config change. One
// since: REMOTE_CAS instance "oss", not "" (rbe-west FU2 confines the worker
// certificate to "oss"-only listeners); and both goldens advertise the
// worker-env platform property (rbe_worker_env_test.go), here
// rbeWorkerEnvSample.
//
// The same program renders the fork tier (WORKER_TIER=fork, rbe-fork-pool.yml),
// always with isolation on: CAS instance oss-fork on :8444, no action cache
// store, nothing uploaded. Its golden is that rendering with the script's own
// isolation keys; the checks below hold whatever the golden says.
func TestRBEWorkerJSONIsolationOffMatchesPreO1(t *testing.T) {
	root := repoRoot(t)
	prog, iso := workerJSONProgram(t, root)
	// render runs the script's own jq program with render()'s arguments for a
	// pool worker; extra adds those render() passes only when they differ from
	// the program's defaults (--argjson zstd true). This is the test's one jq
	// subprocess (the census's Medium owner), so the WireZstd subtest uses it too.
	render := func(t *testing.T, tier, host, isolation string, extra ...string) []byte {
		t.Helper()
		args := []string{
			"-n",
			"--arg", "host", host,
			"--arg", "root", "/home/runner/work/_temp/nl-worker",
			"--arg", "store", "/home/runner/work/_temp/nl-worker",
			"--arg", "name", "pool-worker-1",
			"--argjson", "slots", "8",
			"--arg", "tier", tier,
			"--arg", "worker_env", rbeWorkerEnvSample,
		}
		args = append(args, extra...)
		args = append(args, "--argjson", "isolation", isolation, prog)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "jq", args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("tier %s: jq (needed by %s itself): %v\n%s", tier, rbeWorkerScript, err, stderr.String())
		}
		return out
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
	for _, c := range []struct {
		tier, host, isolation, golden string
	}{
		// The OSS tier, isolation off: the pre-O1 worker.json, byte for byte
		// in content.
		{"oss", "grpcs://rbe-west.example.invalid:443", "{}", "worker-isolation-off.golden.json"},
		{"fork", "grpcs://rbe-fork.example.invalid:8444", iso, "worker-fork.golden.json"},
	} {
		out := render(t, c.tier, c.host, c.isolation)
		golden, err := os.ReadFile(filepath.Join(root, "scripts", "testdata", "rbe-worker", c.golden))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := decode(out), decode(golden); !reflect.DeepEqual(got, want) {
			t.Errorf("tier %s: worker.json differs from %s:\ngot:\n%s\nwant:\n%s", c.tier, c.golden, out, golden)
		}
		if err := checkWorkerJSONAdvertises(out, rbeWorkerEnvSample); err != nil {
			t.Errorf("tier %s: %v", c.tier, err)
		}
		if c.tier == "fork" {
			for _, err := range checkForkWorkerJSON(out, c.host) {
				t.Errorf("tier fork: %v", err)
			}
		}
		for _, err := range checkNoDefaultInstance(out) {
			t.Errorf("tier %s: %v", c.tier, err)
		}
	}

	// RBE_WIRE_ZSTD=1, both tiers: fetches zstd (REMOTE_READ, the read-only
	// side of a fast_slow), uploads identity (REMOTE_WRITE, false explicitly).
	// No writable rbe-west listener accepts zstd, so no other remote store may
	// compress. REMOTE_READ goes to RBE_WIRE_ZSTD_READ_URL, by default the
	// worker's own endpoint, where rbe-west routes this certificate's
	// ByteStream.Read to its read-only zstd process. Off, worker.json is the
	// golden one (above).
	t.Run("WireZstd", func(t *testing.T) {
		const readDefault = `ZSTD_READ_URL=${RBE_WIRE_ZSTD_READ_URL:-grpcs://${RBE_WEST_HOST}:${RBE_WEST_PORT}}`
		// Indented: measure mode (no farm host) skips it.
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(readDefault) + `$`).MatchString(readFile(t, root, rbeWorkerScript)) {
			t.Errorf("%s: want %s (REMOTE_READ defaults to the worker's own endpoint)", rbeWorkerScript, readDefault)
		}
		type store struct {
			Name string `json:"name"`
			GRPC *struct {
				InstanceName string `json:"instance_name"`
				Endpoints    []struct {
					Address string `json:"address"`
				} `json:"endpoints"`
				StoreType  string `json:"store_type"`
				Compressed *bool  `json:"experimental_remote_cache_compression"`
			} `json:"grpc"`
			FastSlow map[string]any `json:"fast_slow"`
		}
		for _, c := range []struct {
			tier, host, isolation, instance, read string
		}{
			{"oss", "grpcs://rbe-west.example.invalid:443", "{}", "oss", ""},
			{"oss", "grpcs://rbe-west.example.invalid:443", "{}", "oss", "grpcs://rbe-west-read.example.invalid:443"},
			{"fork", "grpcs://rbe-fork.example.invalid:8444", iso, "oss-fork", ""},
		} {
			extra := []string{"--argjson", "zstd", "true"}
			wantRead := c.host
			if c.read != "" {
				extra = append(extra, "--arg", "read", c.read)
				wantRead = c.read
			}
			out := render(t, c.tier, c.host, c.isolation, extra...)
			for _, err := range checkNoDefaultInstance(out) {
				t.Errorf("tier %s (zstd): %v", c.tier, err)
			}
			if c.tier == "fork" {
				for _, err := range checkForkWorkerJSON(out, c.host) {
					t.Errorf("fork tier: %v", err)
				}
			}
			var cfg struct {
				Stores []store `json:"stores"`
			}
			if err := json.Unmarshal(out, &cfg); err != nil {
				t.Fatal(err)
			}
			got := map[string]any{}
			for _, s := range cfg.Stores {
				switch {
				case s.GRPC != nil:
					if s.GRPC.Compressed == nil {
						got[s.Name] = nil
					} else {
						got[s.Name] = *s.GRPC.Compressed
					}
					addr := c.host
					if s.Name == "REMOTE_READ" {
						addr = wantRead
					}
					if len(s.GRPC.Endpoints) != 1 || s.GRPC.Endpoints[0].Address != addr {
						t.Errorf("tier %s: store %s endpoints %+v, want %s alone", c.tier, s.Name, s.GRPC.Endpoints, addr)
					}
					if s.GRPC.StoreType == "cas" && s.GRPC.InstanceName != c.instance {
						t.Errorf("tier %s: store %s instance %q, want %q", c.tier, s.Name, s.GRPC.InstanceName, c.instance)
					}
				case s.Name == "REMOTE_CAS":
					got[s.Name] = s.FastSlow["fast_direction"]
				}
			}
			want := map[string]any{"REMOTE_READ": true, "REMOTE_WRITE": false, "REMOTE_CAS": "read_only"}
			if c.tier == "oss" {
				want["REMOTE_AC"] = nil // the action cache never compresses
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("tier %s (read %q) stores %v, want %v", c.tier, c.read, got, want)
			}
		}
	})
}

// workerJSONProgram returns the jq program render() writes worker.json with
// and the script's own isolation='{...}' worker keys.
func workerJSONProgram(t *testing.T, root string) (prog, iso string) {
	t.Helper()
	script := readFile(t, root, rbeWorkerScript)
	m := regexp.MustCompile(`(?s)--argjson isolation "\$isolation" '\n(.*?)' >"\$ROOT/worker.json"\n`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s: no worker.json jq program", rbeWorkerScript)
	}
	i := regexp.MustCompile(`(?s)\n\tisolation='(\{.*?\})'\n`).FindStringSubmatch(script)
	if i == nil {
		t.Fatalf("%s: no isolation='{...}' worker config", rbeWorkerScript)
	}
	return m[1], i[1]
}

// checkNoDefaultInstance checks that no remote store in a worker.json uses
// instance "": rbe-west's edges confine both worker certificates to
// listeners that know "oss" (the OSS tier, :443, FU2) or "oss-fork" (the
// fork tier, :8444) alone, where "" is 'instance_name' not configured.
func checkNoDefaultInstance(out []byte) []error {
	var cfg struct {
		Stores []struct {
			Name string `json:"name"`
			GRPC *struct {
				InstanceName *string `json:"instance_name"`
			} `json:"grpc"`
		} `json:"stores"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return []error{err}
	}
	var errs []error
	for _, s := range cfg.Stores {
		if s.GRPC != nil && (s.GRPC.InstanceName == nil || *s.GRPC.InstanceName == "") {
			errs = append(errs, errors.New("store "+s.Name+" uses instance \"\"; want oss (or oss-fork)"))
		}
	}
	return errs
}

// checkForkWorkerJSON checks a fork-tier worker.json: every endpoint is the
// fork host, the only remote store is the CAS for instance oss-fork, no
// store is an action cache, results are never uploaded, and actions go
// through the isolation entrypoint.
func checkForkWorkerJSON(out []byte, host string) []error {
	var cfg struct {
		Stores []struct {
			Name string `json:"name"`
			GRPC *struct {
				InstanceName string `json:"instance_name"`
				Endpoints    []struct {
					Address string `json:"address"`
				} `json:"endpoints"`
				StoreType  string `json:"store_type"`
				Compressed *bool  `json:"experimental_remote_cache_compression"`
			} `json:"grpc"`
		} `json:"stores"`
		Workers []struct {
			Local struct {
				WorkerAPIEndpoint struct {
					URI string `json:"uri"`
				} `json:"worker_api_endpoint"`
				UploadActionResult map[string]any `json:"upload_action_result"`
				Entrypoint         string         `json:"entrypoint"`
			} `json:"local"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return []error{err}
	}
	var errs []error
	var grpcStores []string
	compressed := map[string]*bool{}
	for _, s := range cfg.Stores {
		if s.GRPC == nil {
			continue
		}
		grpcStores = append(grpcStores, s.Name)
		compressed[s.Name] = s.GRPC.Compressed
		if s.GRPC.StoreType != "cas" || s.GRPC.InstanceName != "oss-fork" {
			errs = append(errs, errors.New("store "+s.Name+": "+s.GRPC.StoreType+" instance "+strconv.Quote(s.GRPC.InstanceName)+", want cas instance \"oss-fork\" only"))
		}
		for _, e := range s.GRPC.Endpoints {
			if e.Address != host {
				errs = append(errs, errors.New("store "+s.Name+" endpoint "+e.Address+", want "+host))
			}
		}
	}
	// RBE_WIRE_ZSTD=1 splits the remote CAS: compressed reads (REMOTE_READ)
	// and identity writes (REMOTE_WRITE, never compressed: no writable
	// rbe-west listener accepts zstd, and a compressed fork upload could store
	// more than it sends).
	switch {
	case reflect.DeepEqual(grpcStores, []string{"REMOTE_CAS"}):
		if c := compressed["REMOTE_CAS"]; c != nil && *c {
			errs = append(errs, errors.New("REMOTE_CAS alone uploads too, so it must not set experimental_remote_cache_compression: true"))
		}
	case reflect.DeepEqual(grpcStores, []string{"REMOTE_READ", "REMOTE_WRITE"}):
		if c := compressed["REMOTE_WRITE"]; c == nil || *c {
			errs = append(errs, errors.New("REMOTE_WRITE must set experimental_remote_cache_compression: false"))
		}
	default:
		errs = append(errs, errors.New("remote stores "+strings.Join(grpcStores, ",")+", want REMOTE_CAS, or REMOTE_READ and REMOTE_WRITE"))
	}
	if len(cfg.Workers) != 1 {
		return append(errs, errors.New("want exactly one worker"))
	}
	w := cfg.Workers[0].Local
	if w.WorkerAPIEndpoint.URI != host {
		errs = append(errs, errors.New("worker API "+w.WorkerAPIEndpoint.URI+", want "+host))
	}
	if !reflect.DeepEqual(w.UploadActionResult, map[string]any{"upload_ac_results_strategy": "never"}) {
		errs = append(errs, errors.New("upload_action_result must be {upload_ac_results_strategy: never} alone"))
	}
	if w.Entrypoint != "/usr/local/libexec/rbe-action/entry" {
		errs = append(errs, errors.New("entrypoint "+strconv.Quote(w.Entrypoint)+": fork actions run isolated only"))
	}
	return errs
}

func TestRBEWorkerScriptGatesNativeLinkOnIsolation(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerScript)
	// Install, prove, then start NativeLink; never the other way round.
	order := []string{
		// Slot uids/gids are free before any slot user is created.
		`taken=$(awk -F: '$3 >= 59000 && $3 <= 59063 { print FILENAME ": " $1 " (" $3 ")" }' /etc/passwd /etc/group)`,
		`[ -z "$taken" ] || fail "uids/gids 59000-59063 must be free for the slot users, taken: $taken"`,
		`sudo groupadd --system --gid "$id" "$u"`,
		`gcc -static -O2 -Wall -Wextra -o "$RUNNER_TEMP/rbe-entry" tools/rbe/rbe-action-entry.c`,
		`gcc -static -O2 -Wall -Wextra -DRBE_ACTION_EXEC -o "$RUNNER_TEMP/rbe-exec" tools/rbe/rbe-action-entry.c`,
		`sudo install -m 0755 tools/rbe/rbe-action-launch "$LIB/launch"`,
		`sudo install -m 0755 tools/rbe/rbe-action-sweep "$LIB/sweep"`,
		`sudo install -m 0755 tools/rbe/rbe-action-selftest "$LIB/selftest"`,
		`sudo tee /etc/rbe-west/rbe-action.env >/dev/null <<-EOF`,
		`sudo chmod 0440 /etc/sudoers.d/rbe-action && sudo visudo -cq`,
		"render\n",
		`if ! sudo "$LIB/selftest" >"$selftest_out"; then`,
		`fail "selftest: $(grep -E '^(FAIL|      )' "$selftest_out" | sed -E 's/^ +[^:]+: / /' | tr -s '\n ' ' ')"`,
		`LC_ALL=C sudo -l -U rbe-a00 2>&1 | grep -q 'not allowed to run sudo'`,
		// What an action can connect to (the selftest's in-action check), not
		// what the host has on /run: the host keeps its sockets.
		`grep -q '^ok    action: no-open-socket' "$selftest_out" ||`,
		`probe "$ROOT/pki/worker.key"`,
		`if ! grep -qE "^uid 590[0-9]{2}$" <<<"$out" || grep -q LEAK <<<"$out"; then`,
		// Mode 1 runs the checks in this shell: any failure ends the worker.
		"\telse\n\t\tisolate\n\tfi\n",
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

// Run 36956951091 (canary): the selftest passed, then eight world-writable
// sockets on Blacksmith's /run (its VM shutdown socket among them) were found
// reachable by actions. MASK_SOCKETS=1 masks them inside each action; MAIN
// keeps the default (0) until it opts in. The sockets phase reads the
// selftest's line, and the launcher and the selftest keep the same sockets.
func TestRBEActionMaskSockets(t *testing.T) {
	root := repoRoot(t)
	launch := readFile(t, root, "tools/rbe/rbe-action-launch")
	selftest := readFile(t, root, "tools/rbe/rbe-action-selftest")
	for _, want := range []string{
		"MASK_SOCKETS=${MASK_SOCKETS:-0}\n",
		`[[ $MASK_SOCKETS == [01] ]] || die "MASK_SOCKETS must be 0 or 1"`,
		`done < <(find "${walk[@]}" -xdev -type s -perm -o+w -print0 2>/dev/null)`,
		`mount --bind /dev/null "$s" 2>/dev/null || [[ ! -S $s ]] || mount --bind /dev/null "$s"`,
	} {
		if !strings.Contains(launch, want) {
			t.Errorf("rbe-action-launch missing %q", want)
		}
	}
	const keep = "case $s in /run/systemd/journal/* | /run/dbus/system_bus_socket) ;; *)"
	if n := strings.Count(launch, keep); n != 1 {
		t.Errorf("rbe-action-launch: %d sockets-kept lists %q, want 1", n, keep)
	}
	if n := strings.Count(selftest, keep); n != 1 {
		t.Errorf("rbe-action-selftest: %d sockets-kept lists %q, want 1 (run_socks, host and action)", n, keep)
	}
	// The action runs the host's run_socks: the same list on both sides.
	if !strings.Contains(selftest, `'"$(declare -f run_socks)"'`) {
		t.Error("rbe-action-selftest: the probe must run the host's run_socks")
	}
	for _, want := range []string{
		`ok() { echo "ok    $1"; }`,
		`ok "action: no-open-socket (${how:-?})"`,
		"elif ((MASK_SOCKETS)); then\n\tbad \"action: no-open-socket",
		`sed -n 's/^S /      world-writable socket the action can connect to: /p' <<<"$out"`,
	} {
		if !strings.Contains(selftest, want) {
			t.Errorf("rbe-action-selftest missing %q", want)
		}
	}
}

// Run 36861390718 died without a word right after the script took world write
// off the image's shared directories: the runner (or Blacksmith's agent)
// relies on them. Isolation leaves the host's permissions alone (the launcher
// makes / and every other mount read-only inside each action, ROOT_RO=1), and
// logs every phase so a silent death still shows where it happened. The two
// runs after that died listing world-writable directories one by one (names
// with spaces in nvm and CodeQL): nothing is listed any more.
func TestRBEWorkerScriptLeavesHostPermissionsAlone(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	for _, bad := range []string{"chmod o-w", "chmod -R", "xargs -r -d '\\n' sudo chmod", "-perm -0002", "RO_DIRS"} {
		if strings.Contains(script, bad) {
			t.Errorf("%s must not change or list the image's world-writable directories (%q); ROOT_RO=1 makes them read-only for actions", rbeWorkerScript, bad)
		}
	}
	at := 0
	for _, phase := range []string{"paths", "packages", "users", "compile", "install", "env", "sudoers", "nft", "render", "selftest", "sudo", "sockets", "probe"} {
		want := "\tphase " + phase + "\n"
		i := strings.Index(script[at:], want)
		if i < 0 {
			t.Fatalf("%s: phase marker %q missing or out of order", rbeWorkerScript, want)
		}
		at += i + len(want)
	}
}

// Five pool-wide rollouts of isolation failed on real Blacksmith runners and
// every worker exited, leaving the OSS pool without workers.
// RBE_ACTION_ISOLATION=canary lets the workers of one run in
// RBE_ACTION_CANARY_EVERY try it and fall back to the rollback instead of
// exiting. This runs blacksmith-worker.sh's own isolation section (LIB= to
// nl=$!) under bash with a sudo that always fails, so isolation fails in its
// packages phase, and a stub NativeLink that keeps the config it was started
// with: a failed canary must start exactly as RBE_ACTION_ISOLATION=0 (the
// pre-O1 worker.json, same slots), a skipped one too, and mode 1 must still
// exit before NativeLink starts.
func TestRBEWorkerIsolationCanary(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerScript)
	from := strings.Index(script, "\nLIB=/usr/local/libexec/rbe-action\n")
	to := strings.Index(script, "\nnl=$!\n")
	if from < 0 || to < from {
		t.Fatalf("%s: no isolation section from LIB= to nl=$!", rbeWorkerScript)
	}
	section := script[from : to+len("\nnl=$!\n")]
	const goldenRoot = "/home/runner/work/_temp/nl-worker"

	type result struct {
		code    int
		out     string
		started []byte // the stub NativeLink's config, nil if it never started
		summary string
	}
	run := func(t *testing.T, mode string, slots int, runID, every string) result {
		t.Helper()
		home := t.TempDir()
		temp := filepath.Join(home, "temp")
		nlRoot := filepath.Join(home, "nl-worker")
		bin := filepath.Join(home, "nl-bin")
		for _, d := range []string{temp, bin, filepath.Join(nlRoot, "work"), filepath.Join(nlRoot, "pki")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(bin, "nativelink"), []byte("#!/bin/sh\ncp \"$1\" \"$1.started\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		prog := "set -euo pipefail\n" +
			"ACTION_ISOLATION=$MODE slots=$SLOTS STORE=$ROOT\n" +
			section + "wait \"$nl\"\n"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "-c", prog)
		cmd.Env = []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + home, "RUNNER_TEMP=" + temp, "ROOT=" + nlRoot, "NL_BIN_DIR=" + bin,
			"RBE_WEST_HOST=rbe-west.example.invalid", "WORKER_NAME=pool-worker-1", "WORKER_ENV=" + rbeWorkerEnvSample,
			"MODE=" + mode, "SLOTS=" + strconv.Itoa(slots), "GITHUB_STEP_SUMMARY=" + filepath.Join(home, "summary.md"),
		}
		if runID != "" {
			cmd.Env = append(cmd.Env, "GITHUB_RUN_ID="+runID)
		}
		if every != "" {
			cmd.Env = append(cmd.Env, "RBE_ACTION_CANARY_EVERY="+every)
		}
		out, err := cmd.CombinedOutput()
		r := result{out: string(out)}
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("bash: %v\n%s", err, out)
			}
			r.code = exit.ExitCode()
		}
		if b, err := os.ReadFile(filepath.Join(nlRoot, "worker.json.started")); err == nil {
			r.started = bytes.ReplaceAll(b, []byte(nlRoot), []byte(goldenRoot))
		}
		if b, err := os.ReadFile(filepath.Join(home, "summary.md")); err == nil {
			r.summary = string(b)
		}
		return r
	}
	golden, err := os.ReadFile(filepath.Join(root, "scripts", "testdata", "rbe-worker", "worker-isolation-off.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	sameJSON := func(a, b []byte) bool {
		var x, y any
		return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
	}

	t.Run("failed canary starts as mode 0", func(t *testing.T) {
		for _, slots := range []int{8, 96} { // 96: above the 64 isolation allows
			plain := run(t, "0", slots, "1004", "4")
			canary := run(t, "canary", slots, "1004", "4")
			if plain.code != 0 || plain.started == nil {
				t.Fatalf("mode 0: exit %d, started %v\n%s", plain.code, plain.started != nil, plain.out)
			}
			if canary.code != 0 || canary.started == nil {
				t.Fatalf("failed canary must not exit and must start NativeLink: exit %d, started %v\n%s", canary.code, canary.started != nil, canary.out)
			}
			if !bytes.Equal(canary.started, plain.started) {
				t.Errorf("slots %d: failed canary worker.json differs from mode 0's:\n%s\nwant:\n%s", slots, canary.started, plain.started)
			}
			if slots == 8 && !sameJSON(canary.started, golden) {
				t.Errorf("failed canary worker.json is not the pre-O1 rendering:\n%s", canary.started)
			}
			for _, want := range []string{
				"isolation: packages\n",
				"::warning title=rbe isolation canary::isolation failed in phase packages: sudo DEBIAN_FRONTEND=noninteractive",
				"\nRBE_ISOLATION_CANARY=failed phase=packages\n",
			} {
				if !strings.Contains(canary.out, want) {
					t.Errorf("failed canary output missing %q:\n%s", want, canary.out)
				}
			}
			if !strings.Contains(canary.summary, "`RBE_ISOLATION_CANARY=failed phase=packages`") {
				t.Errorf("job summary = %q", canary.summary)
			}
		}
	})

	t.Run("mode 1 still exits", func(t *testing.T) {
		r := run(t, "1", 8, "1004", "4")
		if r.code != 1 || r.started != nil || !strings.Contains(r.out, "isolation: packages\n") || strings.Contains(r.out, "RBE_ISOLATION_CANARY") {
			t.Errorf("mode 1 with a failing phase: exit %d (want 1), NativeLink started %v (want false)\n%s", r.code, r.started != nil, r.out)
		}
	})

	// Selection: GITHUB_RUN_ID % RBE_ACTION_CANARY_EVERY == 0 (default 4, run
	// ids decimal even with leading zeros); anything unusable skips with a
	// warning rather than ending the worker.
	for _, c := range []struct {
		runID, every string
		selected     bool
		warn         bool
	}{
		{"1004", "4", true, false},
		{"1003", "4", false, false},
		{"1004", "", true, false},
		{"1002", "", false, false},
		{"7", "1", true, false},
		{"36861390718", "2", true, false},
		{"36861390719", "2", false, false},
		{"010", "8", false, false}, // 10, not octal 8
		{"12", "0", false, true},
		{"12", "x", false, true},
		{"", "4", false, true},
	} {
		t.Run("run "+c.runID+" every "+c.every, func(t *testing.T) {
			r := run(t, "canary", 8, c.runID, c.every)
			if r.code != 0 || r.started == nil {
				t.Fatalf("canary: exit %d, started %v\n%s", r.code, r.started != nil, r.out)
			}
			tried := strings.Contains(r.out, "isolation: paths\n")
			if tried != c.selected {
				t.Errorf("selected = %v, want %v\n%s", tried, c.selected, r.out)
			}
			if !c.selected {
				if !strings.Contains(r.out, "\nRBE_ISOLATION_CANARY=skipped\n") || !strings.Contains(r.summary, "`RBE_ISOLATION_CANARY=skipped`") {
					t.Errorf("not selected: want RBE_ISOLATION_CANARY=skipped in output and summary\n%s\nsummary: %q", r.out, r.summary)
				}
				if !sameJSON(r.started, golden) {
					t.Errorf("skipped canary worker.json is not the pre-O1 rendering:\n%s", r.started)
				}
			}
			if got := strings.Contains(r.out, "::warning title=rbe isolation canary::RBE_ACTION_CANARY_EVERY"); got != c.warn {
				t.Errorf("unusable-setting warning = %v, want %v\n%s", got, c.warn, r.out)
			}
		})
	}
}

// A sticky disk is written by other VMs, so scrub_cas must delete whatever it
// holds that is not a blob named for its own SHA-256 and size, whatever the
// name: quotes and spaces once aborted it under set -e (xargs read them as
// quoting), and -regextype after ! left GNU find 4.9's name filter dead. The
// function runs from the script itself in bash, with sudo as a pass-through
// (chown to the caller's own ids needs no root), on a store whose own path
// has a space and a quote too.
func TestRBEWorkerScrubCAS(t *testing.T) {
	m := regexp.MustCompile(`(?s)\nscrub_cas\(\) \{ # STORE\n.*?\n\}\n`).FindString(readFile(t, repoRoot(t), rbeWorkerScript))
	if m == "" {
		t.Fatalf("%s: no scrub_cas function", rbeWorkerScript)
	}
	tmp := t.TempDir()
	store := filepath.Join(tmp, "sticky disk's")
	d2 := filepath.Join(store, "content", "d2")
	for _, d := range []string{d2, filepath.Join(store, "work", "action 1"), filepath.Join(store, "tmp"), filepath.Join(tmp, "runner")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	blob := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		return hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(body)) + "-7"
	}
	good, open := blob("kept"), blob("kept, world-writable")
	files := map[string]string{
		good:                 "kept",
		open:                 "kept, world-writable",
		blob("hashed") + "x": "hashed", // not the name pattern
		blob("other"):        "OTHER",  // hash mismatch, same size
		strings.Replace(blob("short"), "-5-", "-50-", 1): "short", // wrong size in the name
		"it's a blob":             "single quote",  // aborted xargs
		`say "cheese" now`:        "double quotes", // aborted xargs
		"new\nline " + blob("nl"): "nl",            // newline: a split name
		"back\\slash":             "sha256sum escape",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(d2, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(d2, open), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(d2, blob("link"))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "content", "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// GNU find and coreutils, as on the runner image; exit 77 skips elsewhere.
	prog := "set -euo pipefail\n" +
		"{ find --version | grep -q GNU && command -v sha256sum nproc; } >/dev/null 2>&1 || exit 77\n" +
		"sudo() { \"$@\"; }\n" + m + "scrub_cas \"$1\"\n"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", prog, "bash", store)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "RUNNER_TEMP=" + filepath.Join(tmp, "runner")}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 77 {
		t.Skip("scrub_cas needs GNU find and coreutils (the runner image's)")
	}
	if err != nil {
		t.Fatalf("scrub_cas: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(d2)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, e := range entries {
		kept = append(kept, e.Name())
	}
	want := []string{good, open}
	if good > open {
		want = []string{open, good}
	}
	if !reflect.DeepEqual(kept, want) {
		t.Errorf("d2 kept %q, want %q\n%s", kept, want, out)
	}
	if fi, err := os.Stat(filepath.Join(d2, open)); err != nil || fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("%s: want go-w, got %v %v", open, fi, err)
	}
	for _, gone := range []string{filepath.Join(store, "content", "stray"), filepath.Join(store, "work", "action 1")} {
		if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the scrub (%v)", gone, err)
		}
	}
	if !strings.Contains(string(out), "cas scrub: 2 blobs removed, 2 kept") {
		t.Errorf("scrub summary: want 2 blobs removed (hash, size), 2 kept; got\n%s", out)
	}
}
