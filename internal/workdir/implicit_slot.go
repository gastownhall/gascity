package workdir

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// slotBranchHashLen is the number of hex digits of sha256(slot path) in a
// pool-slot branch name. The hash separates cities that share one repository.
const slotBranchHashLen = 12

// ImplicitSlot is the pool-slot git worktree a numbered instance of a marked
// rig-scoped implicit agent runs in.
type ImplicitSlot struct {
	// Path is the slot worktree root, <city>/.gc/worktrees/<rig>/<template>-<n>.
	Path string
	// RigRoot is the rig checkout the slot is a worktree of.
	RigRoot string
	// Branch is the slot's branch, gc-<template>-<n>-<12 hex of sha256(Path)>.
	Branch string
}

// ImplicitSlotFor reports the pool slot for qualifiedName when a is an agent
// marked with PoolSlotWorktrees, has no work_dir, and qualifiedName is one of
// its numbered instances (<dir>/<template>-<n>, n >= 1 without leading zeros)
// on a rig whose root holds a .git entry. Canonical, ad hoc, alias and other
// identities never match and keep their existing work dir.
//
// It is a pure path computation: the only filesystem access is one Lstat of
// <rigRoot>/.git, and it never runs git.
func ImplicitSlotFor(cityPath, qualifiedName string, a config.Agent, rigs []config.Rig) (ImplicitSlot, bool) {
	if !a.PoolSlotWorktrees || a.WorkDir != "" || a.Dir == "" || !a.SupportsMultipleSessions() {
		return ImplicitSlot{}, false
	}
	template := a.Name
	if a.PoolName != "" {
		_, template = config.ParseQualifiedName(a.PoolName)
	}
	dir, instance := config.ParseQualifiedName(qualifiedName)
	if dir != a.Dir || !isSlotInstanceName(instance, template) {
		return ImplicitSlot{}, false
	}
	rigName := ConfiguredRigName(cityPath, a, rigs)
	if rigName == "" {
		return ImplicitSlot{}, false
	}
	rigRoot := RigRootForName(rigName, rigs)
	if rigRoot == "" {
		return ImplicitSlot{}, false
	}
	rigRoot = ResolveDirPath(cityPath, rigRoot)
	if _, err := os.Lstat(filepath.Join(rigRoot, ".git")); err != nil {
		return ImplicitSlot{}, false
	}
	path := filepath.Join(cityPath, ".gc", "worktrees", rigName, instance)
	sum := sha256.Sum256([]byte(path))
	return ImplicitSlot{
		Path:    path,
		RigRoot: rigRoot,
		Branch:  "gc-" + instance + "-" + hex.EncodeToString(sum[:])[:slotBranchHashLen],
	}, true
}

// isSlotInstanceName reports whether name is <template>-<n> with n a positive
// decimal integer written without leading zeros.
func isSlotInstanceName(name, template string) bool {
	suffix, ok := strings.CutPrefix(name, template+"-")
	if !ok || suffix == "" || suffix[0] == '0' {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, err := strconv.Atoi(suffix)
	return err == nil
}
