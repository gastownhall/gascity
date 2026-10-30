package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/gastownhall/gascity/internal/importsvc"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// resolveRigIncludeImports is rig.Deps.ResolveIncludeImports for the CLI. Every
// remote include passes gc import add's credential-in-URL refusal. A
// version-less, non-bundled remote include gets the constraint gc import add
// would write (importsvc.Deps.ResolveRemoteDefaultVersion through the CLI's
// stubbable import seams), so a source the city already imports or locks
// keeps the city's constraint; a bundled include keeps its canonical pin; a
// local path, or a legacy [packs] source carrying "#ref", keeps no version.
// Bundled and remote includes join the deferred packs.lock commit, so
// gc import check is clean right after the add, except a "#ref" source: it
// stays out of packs.lock and the city does not load until a later sync
// locks it, at a commit chosen without the ref (packman fetches without it,
// so no import honors a "#ref"). Nothing is written here; resolution errors
// abort before rig add mutates anything.
func resolveRigIncludeImports(cityPath string, imports []config.BoundImport) ([]config.BoundImport, func() error, error) {
	resolved := append([]config.BoundImport(nil), imports...)
	svc := importSvcDeps()
	for i := range resolved {
		imp := &resolved[i].Import
		if remotesource.IsRemote(imp.Source) {
			if err := importsvc.RejectSourceUserinfo(imp.Source); err != nil {
				return nil, nil, fmt.Errorf("--include %q: %w: %w", gitcred.RedactUserinfo(imp.Source), importsvc.ErrInvalidSource, err)
			}
		}
		if !includeNeedsDefaultVersion(*imp) {
			continue
		}
		version, err := svc.ResolveRemoteDefaultVersion(fsys.OSFS{}, cityPath, imp.Source)
		if err != nil {
			return nil, nil, fmt.Errorf("--include %q: %w", gitcred.RedactUserinfo(imp.Source), err)
		}
		imp.Version = version
	}
	return composeRigImports(cityPath, resolved, isRigIncludeLockSource)
}

// includeNeedsDefaultVersion reports whether imp is a version-less,
// non-bundled remote import without an embedded "#ref".
func includeNeedsDefaultVersion(imp config.Import) bool {
	return strings.TrimSpace(imp.Version) == "" &&
		remotesource.IsRemote(imp.Source) &&
		!builtinpacks.IsSource(imp.Source) &&
		!hasRepositoryRefInSource(imp.Source)
}

// isRigIncludeLockSource reports whether an --include source belongs in
// packs.lock: a bundled source, or a remote one without an embedded "#ref".
func isRigIncludeLockSource(source string) bool {
	return builtinpacks.IsSource(source) ||
		(remotesource.IsRemote(source) && !hasRepositoryRefInSource(source))
}
