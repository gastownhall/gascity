package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestFirstRootCommandMatchesPersistentScopeGrammar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		word string
		ok   bool
	}{
		{name: "bare", args: nil},
		{name: "metrics", args: []string{"metrics"}, word: "metrics", ok: true},
		{name: "metrics leaf", args: []string{"metrics", "status"}, word: "metrics", ok: true},
		{name: "separate city", args: []string{"--city", "/tmp/city", "metrics", "status"}, word: "metrics", ok: true},
		{name: "equals city", args: []string{"--city=/tmp/city", "metrics"}, word: "metrics", ok: true},
		{name: "separate rig", args: []string{"--rig", "tower", "metrics"}, word: "metrics", ok: true},
		{name: "equals rig", args: []string{"--rig=tower", "metrics"}, word: "metrics", ok: true},
		{name: "separate context", args: []string{"--context", "prod", "metrics"}, word: "metrics", ok: true},
		{name: "equals context", args: []string{"--context=prod", "metrics"}, word: "metrics", ok: true},
		{name: "separate city URL", args: []string{"--city-url", "https://city.example", "metrics"}, word: "metrics", ok: true},
		{name: "equals city URL", args: []string{"--city-url=https://city.example", "metrics"}, word: "metrics", ok: true},
		{name: "separate city name", args: []string{"--city-name", "remote", "metrics"}, word: "metrics", ok: true},
		{name: "equals city name", args: []string{"--city-name=remote", "metrics"}, word: "metrics", ok: true},
		{name: "repeated scopes", args: []string{"--city", "/tmp/a", "--rig=tower", "--city=/tmp/b", "metrics"}, word: "metrics", ok: true},
		{name: "terminator consumed as city value", args: []string{"--city", "--", "metrics"}, word: "metrics", ok: true},
		{name: "terminator consumed as rig value", args: []string{"--rig", "--", "metrics"}, word: "metrics", ok: true},
		{name: "unconsumed terminator", args: []string{"--", "metrics"}},
		{name: "terminator after scope", args: []string{"--city", "/tmp/city", "--", "metrics"}},
		{name: "city consumes metrics", args: []string{"--city", "metrics", "status"}, word: "status", ok: true},
		{name: "rig consumes metrics", args: []string{"--rig", "metrics"}},
		{name: "equals value is not command", args: []string{"--city=metrics", "status"}, word: "status", ok: true},
		{name: "missing city value", args: []string{"--city"}},
		{name: "missing rig value", args: []string{"--rig"}},
		{name: "missing context value", args: []string{"--context"}},
		{name: "missing city URL value", args: []string{"--city-url"}},
		{name: "missing city name value", args: []string{"--city-name"}},
		{name: "unknown long flag fails closed", args: []string{"--format", "metrics"}},
		{name: "unknown short flag fails closed", args: []string{"-v", "metrics"}},
		{name: "lone dash fails closed", args: []string{"-", "metrics"}},
		{name: "first positional wins", args: []string{"status", "metrics"}, word: "status", ok: true},
		{name: "metrics stops later parsing", args: []string{"metrics", "--", "status"}, word: "metrics", ok: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			word, ok := firstRootCommand(test.args)
			if word != test.word || ok != test.ok {
				t.Fatalf("firstRootCommand(%q) = (%q, %t), want (%q, %t)", test.args, word, ok, test.word, test.ok)
			}
		})
	}
}

func TestRootCommandOptionsSkipPackDiscoveryOnlyForMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		skip bool
	}{
		{name: "metrics", args: []string{"metrics"}, skip: true},
		{name: "scoped metrics", args: []string{"--city", "/tmp/city", "--rig=tower", "metrics", "status"}, skip: true},
		{name: "remote context metrics", args: []string{"--context=prod", "metrics", "status"}, skip: true},
		{name: "remote URL metrics", args: []string{"--city-url", "https://city.example", "--city-name=remote", "metrics", "status"}, skip: true},
		{name: "ordinary", args: []string{"status"}},
		{name: "metrics is city value", args: []string{"--city", "metrics", "status"}},
		{name: "after terminator", args: []string{"--", "metrics"}},
		{name: "unknown flag", args: []string{"--unknown", "metrics"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := rootCommandOptionsForArgs(test.args)
			if got := !options.discoverPackCommands; got != test.skip {
				t.Fatalf("rootCommandOptionsForArgs(%q) skip discovery = %t, want %t", test.args, got, test.skip)
			}
			if got := !options.eagerPackCommandDiscovery; got != test.skip {
				t.Fatalf("rootCommandOptionsForArgs(%q) skip eager discovery = %t, want %t", test.args, got, test.skip)
			}
			if len(options.invocationArgs) != len(test.args) {
				t.Fatalf("root options args = %q, want %q", options.invocationArgs, test.args)
			}
		})
	}
}

func TestRootConstructionAlwaysRegistersMetrics(t *testing.T) {
	t.Parallel()

	root := newRootCmdWithOptions(
		&bytes.Buffer{},
		&bytes.Buffer{},
		rootCommandOptionsForArgs([]string{"metrics", "status"}),
	)
	if findSubcommand(root, "metrics") == nil {
		t.Fatal("metrics command is missing from the root command tree")
	}
}

func TestRunOptionsCanDisableAllPackDiscovery(t *testing.T) {
	cityPath, _ := setupPackCity(t)
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })

	args := []string{"mypack", "hello"}
	options := rootCommandOptionsForArgs(args)
	options.discoverPackCommands = false
	options.eagerPackCommandDiscovery = false
	var stdout, stderr bytes.Buffer
	if code := runWithRootCommandOptions(args, &stdout, &stderr, options); code == 0 {
		t.Fatalf("pack command executed with discovery disabled: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "hello from mypack") {
		t.Fatalf("pack command materialized with discovery disabled: stdout=%q", stdout.String())
	}
}

func TestCredentialHelperInvocationUsesInjectedRootGrammar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "get", args: []string{"git-credential", "get"}, want: true},
		{name: "scoped", args: []string{"--city", "/tmp/city", "--rig=tower", "git-credential", "get"}, want: true},
		{name: "city consumes helper", args: []string{"--city", "git-credential", "get"}},
		{name: "terminated", args: []string{"--", "git-credential", "get"}},
		{name: "not first command", args: []string{"status", "git-credential"}},
		{name: "unknown flag", args: []string{"--json", "git-credential", "get"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isCredentialHelperInvocation(test.args); got != test.want {
				t.Fatalf("isCredentialHelperInvocation(%q) = %t, want %t", test.args, got, test.want)
			}
		})
	}
}

func TestRootConstructionUsesInjectedArgsInsteadOfAmbientOSArgs(t *testing.T) {
	cityPath := setupPackExitCity(t)
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })

	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })

	tests := []struct {
		name        string
		ambientArgs []string
		injected    []string
		wantPack    bool
	}{
		{
			name:        "ordinary injected args discover packs",
			ambientArgs: []string{"version"},
			injected:    []string{"version"},
			wantPack:    true,
		},
		{
			name:        "ambient metrics cannot suppress ordinary discovery",
			ambientArgs: []string{"metrics", "status"},
			injected:    []string{"version"},
			wantPack:    true,
		},
		{
			name:        "injected metrics suppresses ordinary ambient discovery",
			ambientArgs: []string{"version"},
			injected:    []string{"metrics", "status"},
			wantPack:    false,
		},
		{
			name:        "ambient credential helper cannot suppress ordinary discovery",
			ambientArgs: []string{"git-credential", "get"},
			injected:    []string{"version"},
			wantPack:    true,
		},
		{
			name:        "injected credential helper suppresses ordinary ambient discovery",
			ambientArgs: []string{"version"},
			injected:    []string{"git-credential", "get"},
			wantPack:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			os.Args = append([]string{oldArgs[0]}, test.ambientArgs...)
			root := newRootCmdWithOptions(&bytes.Buffer{}, &bytes.Buffer{}, rootCommandOptionsForArgs(test.injected))
			if got := findSubcommand(root, "backstage") != nil; got != test.wantPack {
				t.Fatalf("pack discovery = %t, want %t for ambient=%q injected=%q", got, test.wantPack, test.ambientArgs, test.injected)
			}
		})
	}
}

func TestNewRootCmdCompatibilityWrapperNeverConsultsAmbientArgs(t *testing.T) {
	cityPath := setupPackExitCity(t)
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })

	oldArgs := os.Args
	os.Args = []string{oldArgs[0], "git-credential", "get"}
	t.Cleanup(func() { os.Args = oldArgs })

	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	if findSubcommand(root, "backstage") == nil {
		t.Fatal("compatibility root unexpectedly used ambient credential-helper argv to suppress discovery")
	}
}
