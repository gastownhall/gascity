package proxyendpoint

import "path/filepath"

// ConfigOwner reports whether configPath is the sql-server config of a bd
// proxy root whose proxy.pid names a live `bd db-proxy-child` for that same
// root, and returns that proxy's PID. Such a record is proof that bd — not gc
// — owns the sql-server child, so gc's Dolt process cleanup must never kill
// it.
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
