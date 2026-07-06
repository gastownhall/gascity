package packman

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/gitcred"
)

// TestBuildNetworkGitArgsByteIdenticalWhenNoInjection is the headline guarantee:
// with a zero Injection (no credential rule matched), the network runner's argv
// is byte-identical to defaultRunGit's argv, so public clones behave exactly as
// they did before credential injection existed.
func TestBuildNetworkGitArgsByteIdenticalWhenNoInjection(t *testing.T) {
	args := []string{"clone", "--quiet", "https://github.com/org/repo", "/tmp/cache"}

	// defaultRunGit's argv construction, replicated here as the oracle.
	wantArgs := append(baseHardeningGitArgs(), args...)

	got := buildNetworkGitArgs(gitcred.Injection{}, args...)
	if !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("buildNetworkGitArgs with zero injection = %#v\nwant %#v", got, wantArgs)
	}
}

// TestBuildNetworkGitArgsInsertsCfgBeforeSubcommand proves injected credential
// -c flags land after the base hardening and before the git subcommand.
func TestBuildNetworkGitArgsInsertsCfgBeforeSubcommand(t *testing.T) {
	inj := gitcred.Injection{CfgArgs: []string{"-c", "credential.helper=", "-c", "credential.useHttpPath=true"}}
	args := []string{"ls-remote", "--tags", "https://github.com/org/repo"}
	got := buildNetworkGitArgs(inj, args...)

	base := baseHardeningGitArgs()
	if !reflect.DeepEqual(got[:len(base)], base) {
		t.Fatalf("base hardening not preserved at the front: %#v", got[:len(base)])
	}
	// The subcommand must be the tail.
	tail := got[len(got)-len(args):]
	if !reflect.DeepEqual(tail, args) {
		t.Fatalf("subcommand not at the tail: %#v", tail)
	}
	// The injection must sit between them.
	mid := got[len(base) : len(got)-len(args)]
	if !reflect.DeepEqual(mid, inj.CfgArgs) {
		t.Fatalf("injected cfg args not between base and subcommand: %#v", mid)
	}
}
