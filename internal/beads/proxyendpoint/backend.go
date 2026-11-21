package proxyendpoint

import (
	"path/filepath"
	"strings"
)

// Argv shape of the sql-server child bd's proxy supervises.
const (
	// ServerVerb is argv[1] of a Dolt SQL server.
	ServerVerb = "sql-server"
	// ServerConfigFlag carries the config file a Dolt SQL server runs from;
	// bd's proxy hands its child ConfigFileName or RuntimeConfigFileName in the
	// proxy root.
	ServerConfigFlag = "--config"
)

// ConfigOwner reports whether configPath is the sql-server config of a bd
// proxy root whose proxy.pid names a live `bd db-proxy-child` for that same
// root, and returns that proxy's PID. Such a record is proof that bd — not gc
// — owns the sql-server child, so no gc cleanup may kill it.
//
// It answers the protection question, not the admission one. Validate and
// Inspect answer "may gc dial this endpoint", which also demands a schema-2
// record, a birth token and a matching root identity. ConfigOwner asks the
// narrower "is this process bd's", and that answer has to stay yes for a
// record too old or too sparse to dial, because a process gc cannot identify
// well enough to talk to is still a process gc must not kill. That is why the
// record is read through ReadOwnership, which tolerates retyped and unknown
// fields, and why no schema, port, root_id or birth token is required.
//
// Ownership is proven by the recorded process still BEING bd's proxy for this
// root, not by where the root sits: bd resolves the root from
// BEADS_PROXIED_SERVER_ROOT_PATH, then the sidecar's root_path, and only then
// the default <scope>/.beads/dolt, so a directory-name check would unprotect
// every scope with an overridden root.
//
// Liveness alone is not that proof. bd leaves proxy.pid on disk when its proxy
// is SIGKILLed, and quarantines the record only on its next adoption in that
// root — which never happens for an abandoned root. Once the recorded PID is
// reused by any other process, a bare liveness probe would protect the
// orphaned sql-server indefinitely, and a hand-written record naming PID 1
// would protect any server started with that --config. So the PID's argv must
// also run the ChildVerb for THIS root (ArgvRunsChild, ArgvMentionsRoot),
// which is what bd actually execs. argv[0]'s filename is not part of the
// proof: bd launches the child with os.Executable(), so a versioned pin
// (BD_BIN=/opt/beads/bd-1.3.0-rc.2) is a supported shape.
//
// Since bd v1.3.3 (beads#7184) the child can also run from
// RuntimeConfigFileName in the same root after a port move; it is the same
// root and the same live proxy, so it gets the same proof.
//
// A ProcessTable missing Alive or Argv proves nothing. Uncovered: a
// config_path override that puts config.yaml somewhere other than the proxy
// root, where no proxy.pid sibling exists.
func ConfigOwner(configPath string, pt ProcessTable) (int, bool) {
	if configPath == "" || pt.Alive == nil || pt.Argv == nil {
		return 0, false
	}
	if base := filepath.Base(configPath); base != ConfigFileName && base != RuntimeConfigFileName {
		return 0, false
	}
	root := filepath.Dir(filepath.Clean(configPath))
	record, err := ReadOwnership(root)
	if err != nil {
		return 0, false
	}
	if record.Kind != RecordKind || record.PID <= 0 {
		return 0, false
	}
	if !pt.Alive(record.PID) {
		return 0, false
	}
	argv, err := pt.Argv(record.PID)
	if err != nil || !ArgvRunsChild(argv) || !ArgvMentionsRoot(argv, root) {
		return 0, false
	}
	return record.PID, true
}

// ServerOwner reports whether argv is a Dolt SQL server whose --config is
// owned by a live bd proxy per ConfigOwner, and returns that proxy's PID.
//
// argv[0] is ignored, as the operator chooses the dolt binary. Both
// `--config PATH` and `--config=PATH` are read, and every occurrence is
// considered: a repeated flag is a shape bd does not emit, so a server that
// names a proven root in any of them is one gc cannot rule out as bd's, the
// same conservative policy ArgvMentionsRoot applies to the proxy's --root.
// A relative path proves nothing: it resolves against the server's working
// directory, which neither argv nor gc's own working directory reveals.
func ServerOwner(argv []string, pt ProcessTable) (int, bool) {
	if len(argv) < 2 || argv[1] != ServerVerb {
		return 0, false
	}
	for _, value := range argvFlagValues(argv, ServerConfigFlag) {
		configPath := strings.TrimSpace(value)
		if configPath == "" || !filepath.IsAbs(configPath) {
			continue
		}
		if pid, ok := ConfigOwner(configPath, pt); ok {
			return pid, true
		}
	}
	return 0, false
}
