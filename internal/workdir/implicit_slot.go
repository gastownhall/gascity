package workdir

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ImplicitSlot identifies the per-instance worktree assigned to a numbered
// pool instance of a rig-scoped implicit agent. Each instance
// <rig>/<template>-<n> gets its own git worktree, provisioned at session
// start from the rig root, so concurrent instances stop sharing — and
// clobbering — one checkout.
type ImplicitSlot struct {
	// Path is the slot worktree's absolute path,
	// <city>/.gc/worktrees/<rig>/<template>-<n>. The tree is deliberately
	// literal rather than WorktreesRoot-derived: the same four-segment shape
	// the pool-slot repair and session pruner recognize, independent of the
	// env-coupled worktrees-root override.
	Path string
	// RigRoot is the rig repository's absolute path — the worktree's
	// provisioning repository.
	RigRoot string
}

// ImplicitSlotFor reports whether qualifiedName resolves to an implicit pool
// slot worktree for the marked agent a, returning the slot when it does.
//
// A match requires all of:
//
//   - the agent is marked (PoolSlotWorktrees), rig-scoped (Dir set), and has
//     no explicit work_dir — an operator-supplied work_dir always wins;
//   - the qualified name is <Dir>/<template>-<n>, where template is the
//     agent's own name or the base of its PoolName (pool copies), and n is a
//     positive number without leading zeros. Canonical identities (no
//     suffix), ad-hoc sessions, and MCP alt identities never match;
//   - the qualified name's directory prefix is the configured rig's name;
//   - the rig root exists and carries a .git entry (plain Lstat, no git
//     subprocess — this runs on session-start hot paths).
//
// The slot path is <city>/.gc/worktrees/<rig>/<template>-<n>. Any miss
// returns false and the caller falls through to today's resolution.
func ImplicitSlotFor(cityPath, qualifiedName string, a config.Agent, rigs []config.Rig) (ImplicitSlot, bool) {
	if !a.PoolSlotWorktrees || a.WorkDir != "" || strings.TrimSpace(a.Dir) == "" {
		return ImplicitSlot{}, false
	}
	dir, instance := config.ParseQualifiedName(qualifiedName)
	if dir == "" || instance == "" || dir != a.Dir {
		return ImplicitSlot{}, false
	}
	if !implicitPoolSlotInstance(a, instance) {
		return ImplicitSlot{}, false
	}
	rigName := ConfiguredRigName(cityPath, a, rigs)
	if rigName == "" || rigName != dir {
		return ImplicitSlot{}, false
	}
	rigRoot := ResolveDirPath(cityPath, RigRootForName(rigName, rigs))
	if rigRoot == "" || rigRoot == cityPath {
		return ImplicitSlot{}, false
	}
	if _, err := os.Lstat(filepath.Join(rigRoot, ".git")); err != nil {
		return ImplicitSlot{}, false
	}
	return ImplicitSlot{
		Path:    filepath.Join(cityPath, ".gc", "worktrees", rigName, instance),
		RigRoot: rigRoot,
	}, true
}

// implicitPoolSlotInstance reports whether instance is a numbered pool
// instance of the agent's template: <template>-<n> with n a positive decimal
// number written without leading zeros. The template candidates are the
// agent's own name and the base of its PoolName, so both a template agent
// resolving "claude-1" and a pool copy named "claude-1" whose template is
// recorded in PoolName resolve the same slot.
func implicitPoolSlotInstance(a config.Agent, instance string) bool {
	templates := []string{a.Name}
	if base := filepath.Base(strings.TrimSpace(a.PoolName)); base != "" && base != "." && base != string(filepath.Separator) && base != a.Name {
		templates = append(templates, base)
	}
	for _, template := range templates {
		prefix := template + "-"
		if !strings.HasPrefix(instance, prefix) {
			continue
		}
		if isPositiveInstanceNumber(instance[len(prefix):]) {
			return true
		}
	}
	return false
}

// isPositiveInstanceNumber reports whether s is [1-9][0-9]*: a positive
// number with no leading zero, so "-0" and "-01" are not instance numbers
// and never displace canonical identities.
func isPositiveInstanceNumber(s string) bool {
	if s == "" {
		return false
	}
	if s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
