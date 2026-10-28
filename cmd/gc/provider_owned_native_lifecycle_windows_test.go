//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nativeProxiedScope is a proxied scope whose sidecar roots its proxy at root.
func nativeProxiedScope(t *testing.T, root string) map[string]string {
	t.Helper()
	sidecar, err := json.Marshal(map[string]any{"root_path": root, "idle_timeout": -1})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"metadata.json":                   `{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server"}`,
		"config.yaml":                     "issue_prefix: gt\n",
		"proxied_server_client_info.json": string(sidecar),
	}
}

// TestNativeBdProviderProxiedLifecycleRunsTheScriptsBd pins the proxied arm of
// op_provider_owned_lifecycle, including recover's refusal to cycle a proxy
// the scope shares with its city. The sidecar is the JSON bd writes, so the
// Windows roots in it arrive escaped.
func TestNativeBdProviderProxiedLifecycleRunsTheScriptsBd(t *testing.T) {
	n := newNativeBdTest(t)
	cityRoot := filepath.Join(n.city, ".beads", "dolt")
	if err := os.MkdirAll(cityRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	city := n.scope("", nativeProxiedScope(t, cityRoot))
	sharedRelative := n.scope(filepath.Join("rigs", "relative"), nativeProxiedScope(t, `..\..\..\.beads\dolt`))
	sharedRespelled := n.scope(filepath.Join("rigs", "respelled"), nativeProxiedScope(t, strings.ToUpper(cityRoot)))
	own := n.scope(filepath.Join("rigs", "own"), nativeProxiedScope(t, "dolt"))
	if err := os.MkdirAll(filepath.Join(own, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		scope  string
		op     string
		want   [][]string
		shared bool
	}{
		{name: "start", scope: city, op: "start", want: [][]string{{"ping"}}},
		{name: "probe", scope: sharedRelative, op: "probe", want: [][]string{{"ping"}}},
		{name: "stop", scope: city, op: "stop", want: [][]string{{"dolt", "stop"}}},
		{name: "shutdown of a sharing rig", scope: sharedRelative, op: "shutdown", want: [][]string{{"dolt", "stop"}}},
		{name: "recover of the city", scope: city, op: "recover", want: [][]string{{"dolt", "stop"}, {"ping"}}},
		{name: "recover of a rig on its own root", scope: own, op: "recover", want: [][]string{{"dolt", "stop"}, {"ping"}}},
		{name: "recover of a rig sharing the city root", scope: sharedRelative, op: "recover", want: [][]string{{"ping"}}, shared: true},
		{name: "recover of a rig naming the city root in other case", scope: sharedRespelled, op: "recover", want: [][]string{{"ping"}}, shared: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := n.forSubtest(t)
			n.reset()
			stderr, err := n.run(n.environ(tt.scope, staleProviderEnv...), tt.op)
			if err != nil {
				t.Fatalf("%s: %v (stderr %q)", tt.op, err, stderr)
			}
			for _, call := range n.requireArgs(tt.want...) {
				requireCallDir(t, call, tt.scope)
				requireCallEnv(t, call, "GC_BEADS_TRANSPORT", "proxied")
				requireCallEnv(t, call, "BEADS_DOLT_PROXIED_SERVER", "1")
				requireCallEnvAbsent(t, call, providerOwnedBdScrubbedEnv...)
			}
			wantNote := fmt.Sprintf("scope %s shares the city proxy root; recovering by ping only", tt.scope)
			if got := strings.Contains(stderr, wantNote); got != tt.shared {
				t.Fatalf("stderr = %q, want the shared-root note %v", stderr, tt.shared)
			}
		})
	}
}

// TestNativeBdProviderStopWithNoStoreIsDone pins the script's no-store arm:
// a scope with no directory or no .beads has nothing to retire, while a
// starting op still goes to bd, and fails where the script's cd would.
func TestNativeBdProviderStopWithNoStoreIsDone(t *testing.T) {
	n := newNativeBdTest(t)
	storeless := filepath.Join(n.city, "rigs", "storeless")
	if err := os.MkdirAll(storeless, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(n.city, "rigs", "missing")
	for _, scope := range []string{storeless, missing} {
		for _, op := range []string{"stop", "shutdown"} {
			n.reset()
			stderr, err := n.run(n.environ(scope), op)
			if err != nil {
				t.Fatalf("%s of %s: %v", op, scope, err)
			}
			if !strings.Contains(stderr, fmt.Sprintf("scope %s has no beads store; nothing to retire", scope)) {
				t.Fatalf("%s of %s: stderr = %q, want the nothing-to-retire note", op, scope, stderr)
			}
			n.requireArgs()
		}
	}
	n.reset()
	if stderr, err := n.run(n.environ(storeless), "start"); err != nil {
		t.Fatalf("start of a storeless scope: %v (stderr %q)", err, stderr)
	}
	requireCallDir(t, n.requireArgs([]string{"ping"})[0], storeless)
	n.reset()
	stderr, err := n.run(n.environ(missing), "start")
	requireScriptExit(t, err, 1)
	if want := fmt.Sprintf("cd: %s: No such file or directory", missing); strings.TrimSpace(stderr) != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	n.requireArgs()
}

// TestNativeBdProviderRefusesBeforeBd pins every refusal the script makes
// before running bd, with its status.
func TestNativeBdProviderRefusesBeforeBd(t *testing.T) {
	tests := []struct {
		name   string
		env    func(n *nativeBdTest, scope string) []string
		args   []string
		status int
		msg    string
	}{
		{name: "GC_DOLT=skip", env: func(n *nativeBdTest, scope string) []string { return n.environ(scope, "GC_DOLT=skip") }, args: []string{"start"}, status: 2},
		{name: "GC_DOLT=skip for init", env: func(n *nativeBdTest, scope string) []string {
			return n.environ(scope, "GC_DOLT=skip", "GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=local")
		}, args: []string{"init", "SCOPE", "gt"}, status: 2},
		{name: "an op the arm does not handle", env: func(n *nativeBdTest, scope string) []string { return n.environ(scope) }, args: []string{"list"}, status: 2},
		{name: "no op", env: func(n *nativeBdTest, scope string) []string { return n.environ(scope) }, status: 2},
		{name: "no BEADS_DIR", env: func(n *nativeBdTest, scope string) []string { return withoutEnvNames(n.environ(scope), "BEADS_DIR") }, args: []string{"start"}, status: 1, msg: "provider-owned beads operation requires BEADS_DIR"},
		{name: "no city", env: func(n *nativeBdTest, scope string) []string { return withoutEnvNames(n.environ(scope), "GC_CITY_PATH") }, args: []string{"start"}, status: 1, msg: "GC_CITY_PATH not set"},
		{name: "an unknown transport", env: func(n *nativeBdTest, scope string) []string { return n.environ(scope, "GC_BEADS_TRANSPORT=tcp") }, args: []string{"start"}, status: 1, msg: "invalid provider-owned beads transport: tcp"},
		{name: "not provider-owned", env: func(n *nativeBdTest, scope string) []string {
			return withoutEnvNames(n.environ(scope), "GC_BEADS_PROVIDER_OWNED")
		}, args: []string{"start"}, status: 1, msg: "gc-beads-bd: only provider-owned operations run natively; the gc-managed Dolt lifecycle is unavailable on this platform"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNativeBdTest(t)
			scope := n.scope("", nativeDirectLocalScope)
			args := append([]string{}, tt.args...)
			for i, arg := range args {
				if arg == "SCOPE" {
					args[i] = scope
				}
			}
			stderr, err := n.run(tt.env(n, scope), args...)
			requireScriptExit(t, err, tt.status)
			if strings.TrimSpace(stderr) != tt.msg {
				t.Fatalf("stderr = %q, want %q", stderr, tt.msg)
			}
			n.requireArgs()
		})
	}
}

// TestNativeBdProviderFindsBdAsTheScriptsShellWould pins "${BD_BIN:-bd}": a
// bare name is searched on the provider's PATH, whatever Windows calls that
// variable, and never on gc's own; a relative path is taken from the scope.
func TestNativeBdProviderFindsBdAsTheScriptsShellWould(t *testing.T) {
	n := newNativeBdTest(t)
	scope := n.scope("", nativeDirectLocalScope)
	providerBin := filepath.Dir(n.bdBin)
	gcBin := filepath.Join(n.root, "gc-bin")
	writeDecoyBd(t, gcBin)
	t.Setenv("PATH", gcBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	onPath := append(withoutEnvNames(n.environ(scope), "BD_BIN", "PATH"), "Path=relative-dir;"+providerBin)
	if stderr, err := n.run(onPath, "start"); err != nil {
		t.Fatalf("start with bd on the provider's Path: %v (stderr %q)", err, stderr)
	}
	n.requireArgs([]string{"ping"})

	n.reset()
	offPath := append(withoutEnvNames(n.environ(scope), "BD_BIN", "PATH"), "PATH="+filepath.Join(n.root, "empty"))
	stderr, err := n.run(offPath, "start")
	requireScriptExit(t, err, 127)
	if !strings.Contains(stderr, "bd: executable file not found in PATH") {
		t.Fatalf("stderr = %q, want bd reported missing", stderr)
	}
	n.requireArgs()

	n.reset()
	fromScope, err := filepath.Rel(scope, n.bdBin)
	if err != nil {
		t.Fatal(err)
	}
	if stderr, err := n.run(n.environ(scope, "BD_BIN="+fromScope), "start"); err != nil {
		t.Fatalf("start with BD_BIN=%s: %v (stderr %q)", fromScope, err, stderr)
	}
	n.requireArgs([]string{"ping"})
}

// TestNativeBdProviderTracesEachBdRunAsTheScriptDoes pins trace_bd_argv: one
// line-format record per bd run under GC_BD_TRACE, and none once the JSONL
// trace has claimed tracing.
func TestNativeBdProviderTracesEachBdRunAsTheScriptDoes(t *testing.T) {
	n := newNativeBdTest(t)
	scope := n.scope("", nativeDirectLocalScope)
	trace := filepath.Join(n.root, "bd.trace")
	if _, err := n.run(n.environ(scope, "GC_BD_TRACE="+trace), "recover"); err != nil {
		t.Fatalf("recover: %v", err)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("reading trace: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	wants := []string{
		fmt.Sprintf(" source=provider-script subcommand=dolt pid=%d dir=%s args=dolt stop", os.Getpid(), scope),
		fmt.Sprintf(" source=provider-script subcommand=ping pid=%d dir=%s args=ping", os.Getpid(), scope),
	}
	if len(lines) != len(wants) {
		t.Fatalf("trace = %q, want %d records", data, len(wants))
	}
	for i, want := range wants {
		if !strings.HasSuffix(lines[i], want) {
			t.Fatalf("trace record %d = %q, want it to end %q", i, lines[i], want)
		}
	}

	if err := os.Remove(trace); err != nil {
		t.Fatal(err)
	}
	claimed := n.environ(scope, "GC_BD_TRACE="+trace, "GC_BD_TRACE_JSON="+filepath.Join(n.root, "bd.jsonl"))
	if _, err := n.run(claimed, "start"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatalf("line trace written while the JSONL trace claims tracing (stat err %v)", err)
	}
}

// TestNativeBdProviderReadsEnvNamesWithoutCase pins Windows environment
// semantics: a name in any case is the one variable a child process sees.
func TestNativeBdProviderReadsEnvNamesWithoutCase(t *testing.T) {
	n := newNativeBdTest(t)
	scope := n.scope("", nativeDirectLocalScope)
	if _, err := n.run(n.environ(scope, "gc_dolt_host=stale-host", "Beads_Dolt_Proxied_Server=1"), "start"); err != nil {
		t.Fatalf("start: %v", err)
	}
	requireCallEnvAbsent(t, n.requireArgs([]string{"ping"})[0], "GC_DOLT_HOST", "BEADS_DOLT_PROXIED_SERVER")

	n.reset()
	if _, err := n.run(n.environ(scope, "gc_beads_transport=proxied"), "start"); err != nil {
		t.Fatalf("start: %v", err)
	}
	requireCallEnv(t, n.requireArgs([]string{"ping"})[0], "BEADS_DOLT_PROXIED_SERVER", "1")

	n.reset()
	trailing := n.environ(scope, "BEADS_DIR="+filepath.Join(scope, ".beads")+`\`)
	if _, err := n.run(trailing, "start"); err != nil {
		t.Fatalf("start with a trailing separator on BEADS_DIR: %v", err)
	}
	requireCallDir(t, n.requireArgs([]string{"ping"})[0], scope)
}
