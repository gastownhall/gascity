package importsvc

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// TestDepsResolveRemoteDefaultVersion pins the constraint a version-less remote
// add defaults to, and that the same source checks an add applies run before
// any resolver is consulted.
func TestDepsResolveRemoteDefaultVersion(t *testing.T) {
	errPolicy := errors.New("policy refused")
	errTags := errors.New("ls-remote failed")
	city := t.TempDir()

	type calls struct{ registry, version, head int }
	newDeps := func(c *calls, release *packman.RegistryRelease, tags string, tagErr error) Deps {
		return Deps{
			ResolveRegistryRelease: func(string, string) (packman.RegistryRelease, bool, error, error) {
				c.registry++
				if release != nil {
					return *release, true, nil, nil
				}
				return packman.RegistryRelease{}, false, nil, nil
			},
			ResolveVersion: func(string, string, string) (packman.ResolvedVersion, error) {
				c.version++
				if tagErr != nil {
					return packman.ResolvedVersion{}, tagErr
				}
				return packman.ResolvedVersion{Version: tags, Commit: "abc"}, nil
			},
			DefaultConstraint: func(version string) (string, error) {
				return "^" + strings.Join(strings.Split(version, ".")[:2], "."), nil
			},
			ResolveHeadCommit: func(string, string) (string, error) {
				c.head++
				return "deadbeef", nil
			},
		}
	}

	t.Run("registry release wins", func(t *testing.T) {
		var c calls
		got, err := newDeps(&c, &packman.RegistryRelease{Version: "0.4.2"}, "9.9.9", nil).ResolveRemoteDefaultVersion(fsys.OSFS{}, city, "https://example.com/tools.git")
		if err != nil || got != "^0.4" {
			t.Fatalf("got %q, %v; want ^0.4", got, err)
		}
		if c.version != 0 {
			t.Fatalf("tag resolver consulted %d times after a registry release", c.version)
		}
	})

	t.Run("newest semver tag", func(t *testing.T) {
		var c calls
		got, err := newDeps(&c, nil, "1.4.0", nil).ResolveRemoteDefaultVersion(fsys.OSFS{}, city, "https://example.com/tools.git")
		if err != nil || got != "^1.4" {
			t.Fatalf("got %q, %v; want ^1.4", got, err)
		}
	})

	t.Run("no semver tags pins head", func(t *testing.T) {
		var c calls
		got, err := newDeps(&c, nil, "", packman.ErrNoSemverTags).ResolveRemoteDefaultVersion(fsys.OSFS{}, city, "https://example.com/tools.git")
		if err != nil || got != "sha:deadbeef" {
			t.Fatalf("got %q, %v; want sha:deadbeef", got, err)
		}
	})

	t.Run("tag listing failure", func(t *testing.T) {
		var c calls
		_, err := newDeps(&c, nil, "", errTags).ResolveRemoteDefaultVersion(fsys.OSFS{}, city, "https://example.com/tools.git")
		if !errors.Is(err, ErrVersionResolveFailed) || !errors.Is(err, errTags) {
			t.Fatalf("err = %v, want ErrVersionResolveFailed wrapping the cause", err)
		}
	})

	rejected := []struct {
		name   string
		source string
		policy func(string) error
		want   error
		secret string
	}{
		{name: "credential in URL", source: "https://u:hunter2@example.com/r.git", want: ErrInvalidSource, secret: "hunter2"},
		{name: "source policy", source: "https://internal.example/r.git", policy: func(string) error { return errPolicy }, want: errPolicy},
		{name: "embedded ref", source: "https://example.com/r.git#v1", want: ErrInvalidSource},
		{name: "local path", source: "./x", want: ErrInvalidSource},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			var c calls
			d := newDeps(&c, nil, "1.0.0", nil)
			d.SourcePolicy = tc.policy
			_, err := d.ResolveRemoteDefaultVersion(fsys.OSFS{}, city, tc.source)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("err %q leaks the embedded credential", err)
			}
			if c.registry+c.version+c.head != 0 {
				t.Fatalf("resolvers consulted (%+v) for a rejected source", c)
			}
		})
	}
}

// TestDepsResolveRemoteDefaultVersionReusesCityConstraint pins that a
// version-less add of a source the city already imports or locks joins the
// city's pin instead of defaulting to the newest release, so it never moves
// (or conflicts with) the shared packs.lock entry: the first version an
// existing import of the same source declares, else the caret of the locked
// version, else a sha pin of the locked commit. No resolver runs then.
func TestDepsResolveRemoteDefaultVersionReusesCityConstraint(t *testing.T) {
	const source = "https://example.com/tools.git"
	const other = "https://example.com/other.git"
	packImport := func(name, src, version string) string {
		return "[pack]\nname = \"demo\"\nschema = 1\n\n[imports." + name + "]\nsource = \"" + src + "\"\nversion = \"" + version + "\"\n"
	}
	rigImport := func(name, src, version string) string {
		return "[workspace]\nname = \"demo\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"alpha\"\n\n[rigs.imports." + name + "]\nsource = \"" + src + "\"\nversion = \"" + version + "\"\n"
	}
	locked := func(src, version, commit string) map[string]packman.LockedPack {
		return map[string]packman.LockedPack{src: {Version: version, Commit: commit}}
	}
	cases := []struct {
		name    string
		pack    string
		city    string
		lock    map[string]packman.LockedPack
		want    string
		resolve bool // the newest-release default is consulted
	}{
		{name: "declared version wins over the lock", pack: packImport("tools", source, "^0.4"), lock: locked(source, "0.5.1", "c5"), want: "^0.4"},
		{name: "first declared version in synthetic-key order", pack: packImport("tools", source, "^0.4"), city: rigImport("tools", source, "~0.4.1"), want: "^0.4"},
		{name: "rig import declares the version", city: rigImport("kit", source, "~0.4.1"), want: "~0.4.1"},
		{name: "version-less import takes the locked caret", pack: packImport("tools", source, ""), lock: locked(source, "0.4.3", "c4"), want: "^0.4"},
		{name: "lock entry alone", lock: locked(source, "1.2.0", "c1"), want: "^1.2"},
		{name: "sha-pinned lock entry", lock: locked(source, "sha:c0ffee", "c0ffee"), want: "sha:c0ffee"},
		{name: "other sources are not reused", pack: packImport("other", other, "^9.0"), lock: locked(other, "9.0.0", "c9"), want: "^1.4", resolve: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			if tc.pack != "" {
				writeFile(t, filepath.Join(city, "pack.toml"), tc.pack)
			}
			if tc.city != "" {
				writeFile(t, filepath.Join(city, "city.toml"), tc.city)
			}
			if tc.lock != nil {
				if err := packman.WriteLockfile(fsys.OSFS{}, city, &packman.Lockfile{Packs: tc.lock}); err != nil {
					t.Fatalf("WriteLockfile: %v", err)
				}
			}
			resolved := 0
			deps := Deps{
				ResolveRegistryRelease: func(string, string) (packman.RegistryRelease, bool, error, error) {
					resolved++
					return packman.RegistryRelease{}, false, nil, nil
				},
				ResolveVersion: func(string, string, string) (packman.ResolvedVersion, error) {
					resolved++
					return packman.ResolvedVersion{Version: "1.4.0", Commit: "c14"}, nil
				},
				DefaultConstraint: func(string) (string, error) { return "^1.4", nil },
				ResolveHeadCommit: func(string, string) (string, error) {
					resolved++
					return "head", nil
				},
			}
			got, err := deps.ResolveRemoteDefaultVersion(fsys.OSFS{}, city, source)
			if err != nil || got != tc.want {
				t.Fatalf("ResolveRemoteDefaultVersion = %q, %v; want %q", got, err, tc.want)
			}
			if (resolved > 0) != tc.resolve {
				t.Fatalf("newest-release resolvers consulted %d times; want consulted=%v", resolved, tc.resolve)
			}
		})
	}

	t.Run("unreadable packs.lock", func(t *testing.T) {
		city := t.TempDir()
		writeFile(t, filepath.Join(city, packman.LockfileName), "schema = [\n")
		refuse := func() { t.Error("resolver consulted although the city's packs.lock is unreadable") }
		deps := Deps{
			ResolveRegistryRelease: func(string, string) (packman.RegistryRelease, bool, error, error) {
				refuse()
				return packman.RegistryRelease{}, false, nil, nil
			},
			ResolveVersion: func(string, string, string) (packman.ResolvedVersion, error) {
				refuse()
				return packman.ResolvedVersion{Version: "1.4.0", Commit: "c14"}, nil
			},
			ResolveHeadCommit: func(string, string) (string, error) {
				refuse()
				return "head", nil
			},
		}
		_, err := deps.ResolveRemoteDefaultVersion(fsys.OSFS{}, city, source)
		if !errors.Is(err, ErrInstallFailed) || !strings.Contains(err.Error(), packman.LockfileName) {
			t.Fatalf("err = %v, want ErrInstallFailed naming %s", err, packman.LockfileName)
		}
	})
}
