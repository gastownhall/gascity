package rig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// ReadBeadsPrefix reads the issue_prefix from an existing .beads/config.yaml
// in the given rig directory. Returns the prefix and true if found, or empty
// string and false if the file doesn't exist or has no prefix. Checks both
// the underscore form (issue_prefix) and dash form (issue-prefix) since the
// lifecycle code writes both.
func ReadBeadsPrefix(fs fsys.FS, rigPath string) (string, bool) {
	prefix, ok, err := contract.ReadIssuePrefix(fs, filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil || !ok {
		return "", false
	}
	return strings.ToLower(prefix), true
}

// beadsDirContainsStore reports whether beadsPath contains evidence that it
// would be dangerous to initialize over. Either canonical marker is enough to
// stop fresh initialization because partial stores should fail closed; only
// missing marker files are ignored.
func beadsDirContainsStore(fs fsys.FS, beadsPath string) (bool, error) {
	for _, name := range [...]string{"metadata.json", "config.yaml"} {
		path := filepath.Join(beadsPath, name)
		if _, err := fs.Stat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("checking %s: %w", path, err)
		}
	}
	return false, nil
}

// adoptedStorePrefix is the issue prefix an --adopt'ed store records, read
// once during planning. prefix is "" when neither .beads/config.yaml nor the
// store records one, or when err is set; inConfigYAML reports that
// .beads/config.yaml records it, so a rename must change it there too; mode is
// what renaming it in the store takes (storeMode); err is the adopt guard's
// refusal of a store it could not read or whose prefix it will not adopt.
type adoptedStorePrefix struct {
	prefix       string
	inConfigYAML bool
	mode         storeMode
	err          error
}

// storeMode is what renaming an adopted store's prefix takes. bd rename-prefix
// refuses a store in proxied-server mode (proxied), so such a store is renamed
// in server mode, switched there and back with bd migrate
// from-proxied-server-to-server and from-server-to-proxied-server. stuck is
// the clause that says why that switch cannot rename it, "" when it can;
// idleTimeout is the --idle-timeout that gives it back the idle timeout its
// client-info sidecar records, "" for bd's default.
type storeMode struct {
	proxied     bool
	stuck       string
	idleTimeout string
}

// readAdoptedStorePrefix learns the issue prefix of the store an --adopt
// registers, and that store's mode (readStoreMode).
// .beads/config.yaml wins when it records a prefix. Otherwise, when the store
// has a metadata.json and the caller can read stores, the store's own config
// table is asked: bd keeps a store's prefix there and leaves config.yaml a
// commented template. A missing metadata.json is left for the adopt guard to
// report.
func readAdoptedStorePrefix(deps Deps, rigPath string) adoptedStorePrefix {
	mode, err := readStoreMode(deps.FS, rigPath)
	if err != nil {
		return adoptedStorePrefix{err: fmt.Errorf("--adopt: %w", err)}
	}
	if prefix, ok, err := contract.ReadIssuePrefix(deps.FS, filepath.Join(rigPath, ".beads", "config.yaml")); err == nil && ok {
		return adoptablePrefix(rigPath, prefix, true, mode)
	}
	if deps.ReadStorePrefix == nil {
		return adoptedStorePrefix{}
	}
	if _, err := deps.FS.Stat(filepath.Join(rigPath, ".beads", "metadata.json")); err != nil {
		return adoptedStorePrefix{}
	}
	prefix, err := deps.ReadStorePrefix(rigPath)
	if err != nil {
		return adoptedStorePrefix{err: fmt.Errorf("--adopt requires a valid issue_prefix in .beads/config.yaml in %s: reading the store's own issue_prefix: %w", rigPath, err)}
	}
	return adoptablePrefix(rigPath, strings.TrimSpace(prefix), false, mode)
}

// readStoreMode reads the mode of the store at rigPath. Its metadata.json puts
// it in proxied-server mode by the two keys bd decides that by; a missing or
// malformed file reads as not proxied, and one that cannot be read is an error.
// For a proxied-server store it learns whether bd will switch it to server
// mode (proxiedStoreMode).
func readStoreMode(fs fsys.FS, rigPath string) (storeMode, error) {
	beadsDir := filepath.Join(rigPath, ".beads")
	metaPath := filepath.Join(beadsDir, "metadata.json")
	backend, _, err := contract.ReadMetadataBackend(fs, metaPath)
	if err != nil {
		return storeMode{}, fmt.Errorf("reading %s: %w", metaPath, err)
	}
	mode, _, err := contract.ReadDoltMode(fs, metaPath)
	if err != nil {
		return storeMode{}, fmt.Errorf("reading %s: %w", metaPath, err)
	}
	if !contract.IsProxiedDoltMode(backend, mode) {
		return storeMode{}, nil
	}
	return proxiedStoreMode(fs, beadsDir), nil
}

// proxiedClientInfo is the part of a proxied-server store's client-info
// sidecar that switching it to server mode and back depends on: the fields gc
// reads of bd's file (proxyendpoint.Sidecar), and External, set when the
// sidecar names an externally hosted Dolt endpoint.
type proxiedClientInfo struct {
	proxyendpoint.Sidecar
	External *struct{} `json:"external"`
}

// proxiedStoreMode is the mode of the proxied-server store in beadsDir. bd
// migrate from-proxied-server-to-server refuses, in this order, a store whose
// client-info sidecar it cannot read, one with no sidecar, one whose sidecar
// names an externally hosted Dolt endpoint, and one its metadata.json marks
// dolt_team_server (cmd/bd/migrate_dolt_mode.go in bd v1.3.0 and v1.3.2). A
// store whose Dolt root (proxiedStoreRoot) is not its own .beads/dolt is not
// switched either: other scopes can stand on that root, as the rigs gc beads
// city migrate-proxied pointed at the city's root do, so bd dolt stop through
// the store would stop their proxy, and bd rename-prefix in server mode acts
// on the workspace that owns the root. A file this cannot read only makes the
// store stuck: it decides the remedy a refusal names, not whether the adopt
// goes ahead.
func proxiedStoreMode(fs fsys.FS, beadsDir string) storeMode {
	wontSwitch := func(reason string) storeMode {
		return storeMode{proxied: true, stuck: "bd will not switch this one to server mode because " + reason}
	}
	sidecarFile := ".beads/" + proxyendpoint.SidecarFileName
	data, err := fs.ReadFile(proxyendpoint.SidecarPath(beadsDir))
	switch {
	case os.IsNotExist(err):
		return wontSwitch("it has no " + sidecarFile)
	case err != nil:
		return wontSwitch(fmt.Sprintf("its %s cannot be read (%v)", sidecarFile, err))
	}
	var info proxiedClientInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return wontSwitch(fmt.Sprintf("its %s cannot be read (%v)", sidecarFile, err))
	}
	if info.External != nil {
		return wontSwitch("its " + sidecarFile + " names an externally hosted Dolt endpoint")
	}
	data, err = fs.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return wontSwitch(fmt.Sprintf("its .beads/metadata.json cannot be read (%v)", err))
	}
	var meta struct {
		DoltTeamServer bool   `json:"dolt_team_server"`
		DoltDataDir    string `json:"dolt_data_dir"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return wontSwitch(fmt.Sprintf("its .beads/metadata.json cannot be read (%v)", err))
	}
	if meta.DoltTeamServer {
		return wontSwitch("its .beads/metadata.json sets dolt_team_server")
	}
	if root := proxiedStoreRoot(beadsDir, info.Sidecar, meta.DoltDataDir); !pathutil.SamePath(root, filepath.Join(beadsDir, "dolt")) {
		return storeMode{proxied: true, stuck: fmt.Sprintf("switching this one to server mode would not help, because its Dolt root %s is not its own .beads/dolt (bd dolt stop would stop the proxy serving every scope on that root, and bd rename-prefix would act on the workspace that owns it)", root)}
	}
	return storeMode{proxied: true, idleTimeout: idleTimeoutFlag(info.IdleTimeout)}
}

// proxiedStoreRoot is the Dolt root bd serves the proxied-server store in
// beadsDir from, as bd's ResolveProxiedServerRootPath resolves it in a shell
// that sets none of its overrides: the sidecar's root_path, else metadata.json's
// dolt_data_dir (dataDir), else .beads/dolt, a relative path taken from
// beadsDir.
func proxiedStoreRoot(beadsDir string, sidecar proxyendpoint.Sidecar, dataDir string) string {
	if root := sidecar.ResolvedRootPath(beadsDir); root != "" {
		return root
	}
	switch {
	case dataDir == "":
		return filepath.Join(beadsDir, "dolt")
	case filepath.IsAbs(dataDir):
		return filepath.Clean(dataDir)
	default:
		return filepath.Join(beadsDir, dataDir)
	}
}

// idleTimeoutFlag renders a sidecar's idle timeout as the --idle-timeout of bd
// migrate from-server-to-proxied-server that restores it: "0" for never, which
// bd records as a negative duration, the duration for a finite one, and "" for
// bd's default (no idle_timeout, or zero), which that command records when the
// flag is left out.
func idleTimeoutFlag(d *time.Duration) string {
	switch {
	case d == nil || *d == 0:
		return ""
	case *d < 0:
		return "0"
	default:
		return d.String()
	}
}

// adoptablePrefix accepts a recorded prefix that is already lowercase. A
// mixed-case one is refused rather than lowercased, wherever it is recorded:
// gc stores rig prefixes lowercase, and minting under a different case would
// fork the store's bead ID series. The remedy renames the store's prefix
// (renameStorePrefix), or says that bd cannot (cannotRename).
func adoptablePrefix(rigPath, prefix string, inConfigYAML bool, mode storeMode) adoptedStorePrefix {
	lower := strings.ToLower(prefix)
	if prefix == lower {
		return adoptedStorePrefix{prefix: prefix, inConfigYAML: inConfigYAML, mode: mode}
	}
	where := ""
	if inConfigYAML {
		where = " in .beads/config.yaml"
	}
	refusal := fmt.Sprintf("--adopt: the store at %s records issue prefix %q%s; gc stores rig prefixes in lowercase, so adopting it would fork its bead ID series", rigPath, prefix, where)
	if mode.stuck != "" {
		return adoptedStorePrefix{err: fmt.Errorf("%s, and %s, so it cannot be adopted", refusal, mode.cannotRename("this proxied-server store"))}
	}
	return adoptedStorePrefix{err: fmt.Errorf("%s; rename that prefix to %s in the store before adopting it: %s", refusal, lower, renameStorePrefix(rigPath, lower, inConfigYAML, mode))}
}

// newPrefixPlaceholder stands in a remedy for the prefix the operator picks.
const newPrefixPlaceholder = "<new-prefix>"

// renameStorePrefix is how to rename the issue prefix of the store at rigPath,
// whose mode is mode, to newPrefix. Every bd command runs from a shell outside
// any gc session with BEADS_DIR naming that store: bd prefers $BEADS_DIR over
// the working directory, gc exports BEADS_DIR to rig agent sessions, and a gc
// session's BEADS_DOLT_* variables can point bd at another Dolt server, so the
// rename could otherwise reach another store. The path is shell-quoted, so the
// command runs as printed. A proxied-server store is switched to server mode
// for bd rename-prefix and back, with the idle timeout it had; bd dolt start
// is needed because gc's stores set dolt.auto-start false. bd rename-prefix
// leaves .beads/config.yaml alone, and --adopt reads a prefix recorded there
// before the store's own (readAdoptedStorePrefix), so one recorded in
// config.yaml (inConfigYAML) must change there too, or the adopt is refused
// again.
func renameStorePrefix(rigPath, newPrefix string, inConfigYAML bool, mode storeMode) string {
	beadsDir := "BEADS_DIR=" + shellquote.Join([]string{filepath.Join(rigPath, ".beads")})
	var steps string
	if mode.proxied {
		toProxied := "bd migrate from-server-to-proxied-server"
		if mode.idleTimeout != "" {
			toProxied += " --idle-timeout " + mode.idleTimeout
		}
		steps = fmt.Sprintf("bd rename-prefix refuses proxied-server stores, so switch this one to server mode for the rename and back with bd's experimental mode migrations: in a new shell outside any gc session, run `export %s`, then `bd dolt stop`, `bd migrate from-proxied-server-to-server`, `bd dolt start`, `bd rename-prefix %s`, `bd dolt stop` and `%s`", beadsDir, newPrefix, toProxied)
	} else {
		steps = fmt.Sprintf("from a shell outside any gc session, run `%s bd rename-prefix %s`", beadsDir, newPrefix)
	}
	if inConfigYAML {
		steps += ", then change the prefix in .beads/config.yaml to " + newPrefix
	}
	return steps
}

// cannotRename is the clause a refusal states, in place of a rename, for the
// proxied-server store that store names when switching it to server mode
// cannot rename it (stuck): bd rename-prefix refuses proxied-server mode, so
// bd cannot change that store's prefix.
func (m storeMode) cannotRename(store string) string {
	return fmt.Sprintf("bd cannot rename the prefix of %s: bd rename-prefix refuses proxied-server stores, and %s", store, m.stuck)
}
