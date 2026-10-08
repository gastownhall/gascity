package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// resolveRigIncludeImports is rig.Deps.ResolveIncludeImports for the CLI. A
// version-less, non-bundled remote include gets the constraint gc import add
// would write (importsvc.Deps.ResolveRemoteDefaultVersion through the CLI's
// stubbable import seams); a bundled include keeps its canonical pin; a local
// path, or a legacy [packs] source carrying "#ref", keeps no version. Every
// remote include joins the deferred packs.lock commit, so gc import check is
// clean right after the add. Nothing is written here; resolution errors abort
// before rig add mutates anything.
func resolveRigIncludeImports(cityPath string, imports []config.BoundImport) ([]config.BoundImport, func() error, error) {
	resolved := append([]config.BoundImport(nil), imports...)
	svc := importSvcDeps()
	for i := range resolved {
		imp := &resolved[i].Import
		if !includeNeedsDefaultVersion(*imp) {
			continue
		}
		version, err := svc.ResolveRemoteDefaultVersion(cityPath, imp.Source)
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
		!strings.Contains(imp.Source, "#")
}

// isRigIncludeLockSource reports whether an --include source belongs in packs.lock.
func isRigIncludeLockSource(source string) bool {
	return builtinpacks.IsSource(source) || remotesource.IsRemote(source)
}
