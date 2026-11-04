package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// rigStoreIdentity names the bead store a scope opens, independent of the path
// it was reached by: the .beads directory after the scope's redirect, plus the
// project id and the database the directory's metadata binds. Two scopes whose
// identities share any key open the same store. A guarded rig compares by
// identity, not by scope root, because a subdirectory or a redirected
// worktree opens the rig's store under another path, and a rig whose metadata
// names the city's database shares the city's store from its own directory.
type rigStoreIdentity struct {
	// beadsDir is the .beads directory the scope opens: the nearest one at
	// or above the scope root, after its redirect. Empty when none exists.
	beadsDir string
	// own reports whether the scope root itself holds a .beads directory
	// (before any redirect); a rig without one opens a store that is not its
	// own.
	own bool
	// keys are the identity keys; any key in common means one store.
	keys []string
}

// sharesStoreWith reports whether a and b name the same store.
func (a rigStoreIdentity) sharesStoreWith(b rigStoreIdentity) bool {
	for _, k := range a.keys {
		for _, l := range b.keys {
			if k == l {
				return true
			}
		}
	}
	return false
}

// rigStoreIdentityFor reads the identity of the store scopeRoot opens. It
// reads two small files at most (.beads/redirect and metadata.json) and never
// opens a store. scopeRoot must already be resolved (resolveStoreScopeRoot).
func rigStoreIdentityFor(scopeRoot string) rigStoreIdentity {
	var id rigStoreIdentity
	scopeRoot = normalizePathForCompare(scopeRoot)
	if scopeRoot == "" {
		return id
	}
	source := ""
	for dir := scopeRoot; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, ".beads")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			source = candidate
			id.own = dir == scopeRoot
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if source == "" {
		return id
	}
	sourceMeta := readStoreIdentityMetadata(source)
	target := followBeadsRedirect(source)
	id.beadsDir = normalizePathForCompare(target)
	id.keys = append(id.keys, "dir:"+id.beadsDir)
	targetMeta := sourceMeta
	if target != source {
		targetMeta = readStoreIdentityMetadata(target)
	}
	if p := firstNonEmpty(sourceMeta.projectID, targetMeta.projectID); p != "" {
		id.keys = append(id.keys, "project:"+p)
	}
	// The source directory's database survives a redirect (bd's own rule:
	// the redirecting directory keeps its database identity).
	if db := firstNonEmpty(sourceMeta.doltDatabase, targetMeta.doltDatabase); db != "" {
		server := firstNonEmpty(sourceMeta.server, targetMeta.server)
		if server == "" {
			server = "managed"
		}
		id.keys = append(id.keys, "db:"+server+"/"+db)
	}
	if ep := firstNonEmpty(sourceMeta.storageEndpoint, targetMeta.storageEndpoint); ep != "" {
		id.keys = append(id.keys, "storage:"+ep+"/"+firstNonEmpty(sourceMeta.storageDatabase, targetMeta.storageDatabase))
	}
	return id
}

// storeIdentityMetadata is the part of a .beads/metadata.json that names a
// store.
type storeIdentityMetadata struct {
	projectID       string
	doltDatabase    string
	server          string
	storageEndpoint string
	storageDatabase string
}

func readStoreIdentityMetadata(beadsDir string) storeIdentityMetadata {
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return storeIdentityMetadata{}
	}
	var raw struct {
		ProjectID       any    `json:"project_id"`
		DoltDatabase    any    `json:"dolt_database"`
		Host            any    `json:"dolt_server_host"`
		Port            any    `json:"dolt_server_port"`
		Socket          any    `json:"dolt_server_socket"`
		StorageEndpoint any    `json:"storage_endpoint"`
		StorageDatabase any    `json:"storage_database"`
		Backend         string `json:"backend"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return storeIdentityMetadata{}
	}
	meta := storeIdentityMetadata{
		projectID:       identityString(raw.ProjectID),
		doltDatabase:    identityString(raw.DoltDatabase),
		storageEndpoint: identityString(raw.StorageEndpoint),
		storageDatabase: identityString(raw.StorageDatabase),
	}
	if socket := identityString(raw.Socket); socket != "" {
		meta.server = "socket:" + socket
	} else if host := identityString(raw.Host); host != "" {
		meta.server = "tcp:" + host + ":" + identityString(raw.Port)
	} else if state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(beadsDir, "config.yaml")); err == nil && ok {
		// gc records its own endpoints in config.yaml.
		if socket := strings.TrimSpace(state.DoltSocket); socket != "" {
			meta.server = "socket:" + socket
		} else if host := strings.TrimSpace(state.DoltHost); host != "" {
			meta.server = "tcp:" + host + ":" + strings.TrimSpace(state.DoltPort)
		}
	}
	return meta
}

// identityString renders a metadata value that names a store: strings as
// given (trimmed), numbers in decimal, anything else as nothing.
func identityString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

// followBeadsRedirect follows one .beads/redirect the way bd does: the first
// non-comment line, relative paths resolved from the directory holding the
// .beads directory, one level only. A redirect that names no existing
// directory is ignored, as bd ignores it.
func followBeadsRedirect(beadsDir string) string {
	data, err := os.ReadFile(filepath.Join(beadsDir, "redirect"))
	if err != nil {
		return beadsDir
	}
	target := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			target = line
			break
		}
	}
	if target == "" {
		return beadsDir
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(beadsDir), target)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		return beadsDir
	}
	return target
}
