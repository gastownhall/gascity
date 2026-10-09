package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/remotesource"
)

const importVersionPinsCheckName = "import-version-pins"

// importVersionPinsDoctorCheck reports remote imports that declare no
// version. Such an import has no constraint of its own: the next install or
// upgrade may lock it to a different commit, so what the city ran yesterday
// cannot be reproduced from its config. Bundled sources are included: gc
// writes their canonical pin when it adds them, so a blank version on one was
// written by hand or by an older gc. Local path imports have no version and
// are skipped.
//
// The check is advisory: some deployments float an import on purpose.
// --fix writes the constraint gc import add would write for the same source,
// which for a source the city already locks is the constraint matching its
// packs.lock entry, so the locked commit does not move.
type importVersionPinsDoctorCheck struct {
	cityPath string
	// resolveDefault returns the constraint to pin source to. Production uses
	// importsvc's default-version resolution through the CLI seams.
	resolveDefault func(cityPath, source string) (string, error)
}

func newImportVersionPinsDoctorCheck(cityPath string) *importVersionPinsDoctorCheck {
	return &importVersionPinsDoctorCheck{
		cityPath: cityPath,
		resolveDefault: func(cityPath, source string) (string, error) {
			return importSvcDeps().ResolveRemoteDefaultVersion(fsys.OSFS{}, cityPath, source)
		},
	}
}

// Name implements doctor.Check.
func (*importVersionPinsDoctorCheck) Name() string { return importVersionPinsCheckName }

// CanFix implements doctor.Check.
func (*importVersionPinsDoctorCheck) CanFix() bool { return true }

// WarmupEligible implements doctor.Check.
func (*importVersionPinsDoctorCheck) WarmupEligible() bool { return false }

// importVersionPinsPayload is the --json payload of import-version-pins.
type importVersionPinsPayload struct {
	Unpinned []unpinnedImport `json:"unpinned"`
}

// unpinnedImport is one remote import that declares no version. Import is the
// scoped key ("pack:<name>", "default-rig:<name>", "rig:<rig>:<name>").
type unpinnedImport struct {
	Import       string `json:"import"`
	Source       string `json:"source"`
	LockedCommit string `json:"locked_commit,omitempty"`
	// Fixable is false for a source carrying a "#ref", which --fix cannot pin:
	// a version cannot be combined with an embedded ref.
	Fixable bool `json:"fixable"`
}

// Run implements doctor.Check.
func (c *importVersionPinsDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: importVersionPinsCheckName, Severity: doctor.SeverityAdvisory}
	unpinned, err := c.unpinned()
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("reading declared imports: %v", err)
		return r
	}
	if len(unpinned) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "every remote import declares a version"
		return r
	}
	r.Payload = importVersionPinsPayload{Unpinned: unpinned}
	for _, u := range unpinned {
		locked := "not in packs.lock"
		if u.LockedCommit != "" {
			locked = "locked at " + u.LockedCommit
		}
		note := "no version declared"
		if !u.Fixable {
			note = "no version declared; the source embeds a #ref, which no install honors — move the ref into version"
		}
		r.Details = append(r.Details, fmt.Sprintf("unpinned | %s | %s | %s; %s", u.Import, u.Source, note, locked))
	}
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("%d remote import(s) declare no version and float between installs", len(unpinned))
	r.FixHint = `run "gc doctor --fix" to pin each to the constraint gc import add would write (its packs.lock entry when locked), or set version by hand`
	return r
}

// Fix implements doctor.Check. It pins every fixable unpinned import in every
// scope that declares its source without a version. Resolution happens for
// all sources before any file is written.
func (c *importVersionPinsDoctorCheck) Fix(_ *doctor.CheckContext) error {
	unpinned, err := c.unpinned()
	if err != nil {
		return fmt.Errorf("reading declared imports: %w", err)
	}
	pins := make(map[string]string)
	var refs []string
	for _, u := range unpinned {
		if !u.Fixable {
			refs = append(refs, u.Import)
			continue
		}
		if _, done := pins[u.Source]; done {
			continue
		}
		version, err := c.resolveDefault(c.cityPath, u.Source)
		if err != nil {
			return fmt.Errorf("resolving a version for import %q (%s): %w", u.Import, u.Source, err)
		}
		if strings.TrimSpace(version) == "" {
			return fmt.Errorf("resolving a version for import %q (%s): resolved an empty constraint", u.Import, u.Source)
		}
		pins[u.Source] = version
	}
	if len(pins) > 0 {
		err := rewriteDeclaredImportsFS(fsys.OSFS{}, c.cityPath, func(imports map[string]config.Import) bool {
			changed := false
			for name, imp := range imports {
				if strings.TrimSpace(imp.Version) != "" {
					continue
				}
				if version, ok := pins[imp.Source]; ok {
					imp.Version = version
					imports[name] = imp
					changed = true
				}
			}
			return changed
		})
		if err != nil {
			return err
		}
	}
	if len(refs) > 0 {
		return errors.New("imports with an embedded #ref need a manual edit (move the ref into version): " + strings.Join(refs, ", "))
	}
	return nil
}

func (c *importVersionPinsDoctorCheck) unpinned() ([]unpinnedImport, error) {
	imports, err := collectAllImportsFS(c.cityPath)
	if err != nil {
		return nil, err
	}
	lock, err := readImportLockfile(fsys.OSFS{}, c.cityPath)
	if err != nil {
		return nil, fmt.Errorf("reading packs.lock: %w", err)
	}
	var out []unpinnedImport
	for key, imp := range imports {
		if !importFloats(imp) {
			continue
		}
		u := unpinnedImport{
			Import:  key,
			Source:  imp.Source,
			Fixable: !hasRepositoryRefInSource(imp.Source),
		}
		if locked, ok := lock.Packs[imp.Source]; ok {
			u.LockedCommit = strings.TrimSpace(locked.Commit)
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Import < out[j].Import })
	return out, nil
}

// importFloats reports whether imp is a remote import that declares no
// version.
func importFloats(imp config.Import) bool {
	return strings.TrimSpace(imp.Version) == "" && remotesource.IsRemote(imp.Source)
}
