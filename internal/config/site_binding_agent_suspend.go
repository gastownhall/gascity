package config

import (
	"errors"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ErrSurgicalAgentEditUnsupported indicates the on-disk [[agent]] block
// identified by identity could not be unambiguously located in city.toml, so
// a byte-preserving surgical edit cannot proceed. Callers must not treat
// this as success: fall back to a full rewrite (accepting comment loss) or
// surface the error, but never silently skip the mutation.
var ErrSurgicalAgentEditUnsupported = errors.New("surgical agent suspend/resume edit not supported for this city.toml")

// WriteCityAgentSuspendedForEdit toggles the suspended value for the inline
// [[agent]] block matching identity (see ParseQualifiedName/AgentMatchesIdentity)
// directly in the on-disk city.toml bytes, preserving every other byte --
// comments, key order, and formatting all survive untouched. cfg is the
// already-mutated config, used only to keep rig site bindings in sync with
// the whole-file rewrite path; the agent mutation itself is applied to the
// raw on-disk bytes, not re-serialized from cfg.
//
// It returns an error wrapping ErrSurgicalAgentEditUnsupported when the
// target [[agent]] block cannot be unambiguously located in the raw text.
//
// TODO(ga-gc16k3): stub delegates to the lossy full-rewrite path pending the
// surgical implementation; this reproduces today's comment-dropping bug so
// the RED tests fail for the right reason.
func WriteCityAgentSuspendedForEdit(fs fsys.FS, tomlPath string, cfg *City, _ string, _ bool) error {
	return writeCityAndRigSiteBindingsForEdit(fs, tomlPath, cfg, nil)
}
