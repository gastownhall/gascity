package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// gc identity is 100% ambient environment: `gc prime --hook` renders a role
// prompt for whatever GC_ALIAS/GC_AGENT/GC_TEMPLATE the process inherited, and
// `gc mail`/`gc bd` follow GC_SESSION_ID/BEADS_DIR the same way. That is safe
// only while the environment belongs to the session the hook is firing for.
//
// It does not always. Claude Code >= 2.1.x runs ONE per-user background daemon
// (lock: $CLAUDE_CONFIG_DIR/daemon.lock, sockets: /tmp/cc-daemon-<uid>/<hash of
// CLAUDE_CONFIG_DIR>/), and that daemon inherits the environment of whichever
// session happened to spawn it. It then hosts background sessions for EVERY
// other session on the box, so a background session opened by agent B can carry
// agent A's GC_* environment while running in agent B's working directory. The
// hook then renders A's role prompt inside B's session, and every subsequent gc
// command in it acts as A — wrong ledger, wrong mail identity, wrong rig.
//
// Hook payloads carry the truth the environment lost: Claude Code delivers
// `cwd` on stdin. When the reported cwd sits inside a DIFFERENT rig's tree than
// the ambient identity claims, the environment is not this session's and the
// prompt must not be rendered.
//
// The classifier is deliberately asymmetric. Refusal requires positive evidence
// of a foreign owner (the cwd resolves inside another configured rig's root);
// a cwd that simply matches nothing known is reported as unknown and only
// warned about, because agents legitimately work in scratch dirs, source
// checkouts, and mounts that no rig root covers. Refusing on absence of
// evidence would break far more sessions than the leak it guards against.

// primeIdentityVerdict is the result of comparing a hook-reported cwd against
// the roots the ambient GC_* identity is entitled to.
type primeIdentityVerdict string

const (
	// primeIdentityOK means the cwd belongs to this identity (or no cwd was
	// reported, so there is nothing to contradict the environment).
	primeIdentityOK primeIdentityVerdict = "ok"
	// primeIdentityUnknown means the cwd matched no known root. Warn, render.
	primeIdentityUnknown primeIdentityVerdict = "unknown"
	// primeIdentityForeign means the cwd resolves inside another rig's tree.
	// The ambient environment is another session's. Refuse.
	primeIdentityForeign primeIdentityVerdict = "foreign"
)

// primeIdentityRoot is one directory the classifier can attribute to an owner.
type primeIdentityRoot struct {
	// Path is the directory, already normalised for comparison.
	Path string
	// Self is true when the root belongs to the ambient identity.
	Self bool
	// Label names the owner for the refusal message (rig name, or the env var
	// the root came from).
	Label string
}

// normalizeIdentityPath makes a path comparable: absolute, symlink-resolved,
// cleaned. Symlink resolution is load-bearing, not cosmetic — gc agent homes
// are routinely reached through a symlinked prefix (a city whose
// .gc/worktrees lives on another volume resolves to a different real path than
// the configured one), so a plain string compare marks healthy polecats
// foreign. Paths that cannot be resolved (not yet created, permission denied)
// fall back to the cleaned absolute form so the caller still gets a usable
// comparison instead of an empty string.
func normalizeIdentityPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}

// identityPathWithin reports whether child is parent or sits underneath it.
// Both arguments must already be normalised. The filepath.Rel form is used
// rather than a string prefix so that "/a/bc" is not treated as inside "/a/b".
func identityPathWithin(child, parent string) bool {
	if child == "" || parent == "" {
		return false
	}
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// classifyPrimeHookCwd attributes a hook-reported cwd to an owner.
//
// Roots nest — every rig's polecat worktrees live under the city directory, so
// the city root contains other rigs' trees. Matching therefore picks the
// LONGEST matching root rather than the first, so the most specific owner wins
// and a city-level self root cannot launder another rig's worktree into "ok".
func classifyPrimeHookCwd(hookCwd string, roots []primeIdentityRoot) (primeIdentityVerdict, string) {
	cwd := normalizeIdentityPath(hookCwd)
	if cwd == "" {
		// No cwd in the payload (older provider, or a non-Claude hook format):
		// no evidence either way, so do not interfere.
		return primeIdentityOK, ""
	}
	best := -1
	for i, root := range roots {
		if root.Path == "" || !identityPathWithin(cwd, root.Path) {
			continue
		}
		if best < 0 || len(root.Path) > len(roots[best].Path) {
			best = i
		}
	}
	if best < 0 {
		return primeIdentityUnknown, ""
	}
	if roots[best].Self {
		return primeIdentityOK, roots[best].Label
	}
	return primeIdentityForeign, roots[best].Label
}

// primeIdentityRootsFromEnv builds the root table for the ambient identity.
//
// Self roots come from the session environment (the agent's own work dir, rig
// root, bead scope) plus the configured path of the rig the environment names.
// Foreign roots are every OTHER configured rig's repository path and that rig's
// worktree tree under the city. getenv is injected so the table is testable
// without mutating the process environment.
func primeIdentityRootsFromEnv(cfg *config.City, cityPath string, getenv func(string) string) []primeIdentityRoot {
	if getenv == nil {
		return nil
	}
	selfRig := strings.TrimSpace(getenv("GC_RIG"))

	var roots []primeIdentityRoot
	addRoot := func(path string, self bool, label string) {
		norm := normalizeIdentityPath(path)
		if norm == "" {
			return
		}
		for _, existing := range roots {
			if existing.Path == norm {
				return
			}
		}
		roots = append(roots, primeIdentityRoot{Path: norm, Self: self, Label: label})
	}

	// The session's own declared working roots. GC_DIR is the agent home (a
	// polecat's per-bead worktrees hang off it); GC_RIG_ROOT and
	// GC_BEADS_SCOPE_ROOT are the rig repo as the session sees it.
	for _, key := range []string{"GC_DIR", "GC_RIG_ROOT", "GC_BEADS_SCOPE_ROOT"} {
		addRoot(getenv(key), true, key)
	}

	if cfg != nil {
		for _, rig := range cfg.Rigs {
			name := strings.TrimSpace(rig.Name)
			isSelf := selfRig != "" && name == selfRig
			label := name
			if label == "" {
				label = "unnamed rig"
			}
			addRoot(rig.Path, isSelf, label)
			if cityPath != "" && name != "" {
				// Pool/polecat worktrees: <city>/.gc/worktrees/<rig>/...
				addRoot(filepath.Join(cityPath, ".gc", "worktrees", name), isSelf, label)
			}
		}
	}

	// The city directory itself is a legitimate place for city-scoped agents
	// (mayor, deacon) to sit. It is added LAST and is the shortest match, so
	// the longest-match rule keeps a foreign rig's worktree underneath it
	// classified as foreign rather than swallowed by this entry.
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH"} {
		addRoot(getenv(key), true, key)
	}
	addRoot(cityPath, true, "city")

	return roots
}

// primeIdentityMismatchPrompt is the text rendered in place of a role prompt
// when the hook cwd proves the ambient environment belongs to another session.
// It is phrased as an instruction to the reading agent because that is the only
// actor that can stop the damage: the process already holds the wrong
// environment, and every gc command it runs will act on the wrong rig.
func primeIdentityMismatchPrompt(envIdentity, envRig, hookCwd, foreignLabel string) string {
	if envIdentity == "" {
		envIdentity = "(unset)"
	}
	if envRig == "" {
		envRig = "(unset)"
	}
	var b strings.Builder
	b.WriteString("# ⛔ IDENTITY MISMATCH — NO ROLE PROMPT RENDERED\n\n")
	b.WriteString("`gc prime --hook` refused to render a role prompt for this session.\n\n")
	fmt.Fprintf(&b, "- Ambient environment claims: **%s** (rig `%s`)\n", envIdentity, envRig)
	fmt.Fprintf(&b, "- This session is actually running in: `%s`\n", hookCwd)
	fmt.Fprintf(&b, "- That directory belongs to: **%s**\n\n", foreignLabel)
	b.WriteString("The GC_* environment in this process is another session's. This happens when a\n")
	b.WriteString("background session is hosted by a shared per-user daemon that inherited a\n")
	b.WriteString("different session's environment.\n\n")
	b.WriteString("**Do not run `gc` or `bd` commands in this session.** They would read and write\n")
	b.WriteString("the wrong rig's ledger and send mail under the wrong agent identity.\n\n")
	b.WriteString("Report this to the operator, then stop.\n")
	return b.String()
}

// warnPrimeIdentityUnknown notes a cwd that matched no known root. Diagnostic
// only: it never suppresses the prompt.
func warnPrimeIdentityUnknown(stderr io.Writer, envIdentity, hookCwd string) {
	if stderr == nil || hookCwd == "" {
		return
	}
	fmt.Fprintf(stderr, "gc prime: hook cwd %q is outside every known rig and city root for identity %q; rendering prompt anyway\n", hookCwd, envIdentity) //nolint:errcheck // best-effort stderr
}
