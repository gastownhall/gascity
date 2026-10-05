package importsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// registryPin is the registry release a semver --version resolved to. The
// manifest records it as "sha:<commit>"; the hash is checked against the
// fetched content before anything is written.
type registryPin struct {
	// pack names the release for messages, e.g. "main:gascity".
	pack    string
	version string
	commit  string
	hash    string
}

func (p *registryPin) String() string {
	return fmt.Sprintf("%s %s", p.pack, p.version)
}

// lookupRegistryPacks is the production LookupRegistryPacks seam: every
// registry configured under the Gas City home, cached catalogs first.
func lookupRegistryPacks(source string) (packregistry.PackLookup, error) {
	return packregistry.LookupPacksBySource(context.Background(), gchome.Default(), source)
}

// verifyRegistryRelease is the production VerifyRegistryRelease seam. It runs
// after lock sync has put source@commit in the shared repo cache and checks
// the pack content there against the registry release hash. A git checkout is
// hashed from its tree at commit; the synthetic cache gc materializes from its
// embedded packs (a bundled source at its canonical pin) has no git history,
// so its files are hashed directly with the same manifest format.
func verifyRegistryRelease(source, commit, hash string) error {
	cachePath, err := packman.RepoCachePath(source, commit)
	if err != nil {
		return err
	}
	subpath := remotesource.Parse(source).Subpath
	if _, err := os.Stat(filepath.Join(cachePath, ".git")); err == nil {
		return packregistry.VerifyPackContentHash(cachePath, commit, subpath, hash)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking repo cache %q: %w", cachePath, err)
	}
	return packregistry.VerifyPackDirContentHash(filepath.Join(cachePath, filepath.FromSlash(subpath)), hash)
}

// resolveRegistryRelease answers a semver --version for a source that a
// configured registry publishes. Registry packs are versioned by their catalog
// release entries, not by git tags (a pack repository can hold many packs and
// tag none of them, or carry repository-level tags unrelated to any one
// pack), so the release entry is the only authority on which commit a pack
// version names. It returns a nil pin when no registry publishes source; the
// caller then keeps the git-tag path. unavailable reports registries that
// could not be read when nothing matched, so a later tag failure can say the
// source may be a registry pack.
func (d Deps) resolveRegistryRelease(source, constraint string) (pin *registryPin, unavailable error, err error) {
	lookup, err := d.lookupRegistryPacks()(source)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading pack registries: %w", ErrVersionResolveFailed, err)
	}
	if len(lookup.Matches) == 0 {
		return nil, errors.Join(lookup.Unavailable...), nil
	}
	pin, err = selectRegistryRelease(lookup.Matches, constraint)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrVersionResolveFailed, err)
	}
	return pin, nil, nil
}

// selectRegistryRelease picks the highest non-withdrawn release satisfying
// constraint across every registry entry that publishes the source. The same
// version published with a different commit or hash by two registries is
// ambiguous and refused rather than guessed.
func selectRegistryRelease(matches []packregistry.PackMatch, constraint string) (*registryPin, error) {
	type candidate struct {
		pack    string
		release packregistry.CatalogRelease
	}
	byVersion := map[string][]candidate{}
	withdrawn := map[string]string{}
	var names []string
	for _, match := range matches {
		name := match.Registry + ":" + match.Pack.Name
		names = append(names, name)
		for _, release := range match.Pack.Releases {
			if release.Withdrawn {
				withdrawn[release.Version] = release.WithdrawnReason
				continue
			}
			byVersion[release.Version] = append(byVersion[release.Version], candidate{pack: name, release: release})
		}
	}
	label := strings.Join(names, ", ")
	versions := make([]string, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, version)
	}
	version, ok := packman.SelectVersion(versions, constraint)
	if !ok {
		msg := fmt.Sprintf("registry pack %s has no release matching %q", label, constraint)
		if reason, isWithdrawn := withdrawn[strings.TrimSpace(constraint)]; isWithdrawn {
			msg += fmt.Sprintf("; %s was withdrawn", strings.TrimSpace(constraint))
			if reason != "" {
				msg += ": " + reason
			}
		}
		available := packman.SortVersions(versions)
		if len(available) == 0 {
			return nil, fmt.Errorf("%s (it publishes no active releases)", msg)
		}
		return nil, fmt.Errorf("%s (available: %s)", msg, strings.Join(available, ", "))
	}
	candidates := byVersion[version]
	first := candidates[0]
	for _, other := range candidates[1:] {
		if other.release.Commit != first.release.Commit || other.release.Hash != first.release.Hash {
			conflicts := make([]string, 0, len(candidates))
			for _, c := range candidates {
				conflicts = append(conflicts, fmt.Sprintf("%s@%s", c.pack, c.release.Commit))
			}
			sort.Strings(conflicts)
			return nil, fmt.Errorf("release %s is published with different content by %s; import with --version sha:<commit> to choose one", version, strings.Join(conflicts, ", "))
		}
	}
	return &registryPin{
		pack:    first.pack,
		version: version,
		commit:  first.release.Commit,
		hash:    first.release.Hash,
	}, nil
}
