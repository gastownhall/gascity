package importsvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/packman"
)

// TestDepsResolveRemoteDefaultVersion pins the constraint a version-less remote
// add defaults to, and that the same source checks an add applies run before
// any resolver is consulted.
func TestDepsResolveRemoteDefaultVersion(t *testing.T) {
	errPolicy := errors.New("policy refused")
	errTags := errors.New("ls-remote failed")

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
		got, err := newDeps(&c, &packman.RegistryRelease{Version: "0.4.2"}, "9.9.9", nil).ResolveRemoteDefaultVersion("/city", "https://example.com/tools.git")
		if err != nil || got != "^0.4" {
			t.Fatalf("got %q, %v; want ^0.4", got, err)
		}
		if c.version != 0 {
			t.Fatalf("tag resolver consulted %d times after a registry release", c.version)
		}
	})

	t.Run("newest semver tag", func(t *testing.T) {
		var c calls
		got, err := newDeps(&c, nil, "1.4.0", nil).ResolveRemoteDefaultVersion("/city", "https://example.com/tools.git")
		if err != nil || got != "^1.4" {
			t.Fatalf("got %q, %v; want ^1.4", got, err)
		}
	})

	t.Run("no semver tags pins head", func(t *testing.T) {
		var c calls
		got, err := newDeps(&c, nil, "", packman.ErrNoSemverTags).ResolveRemoteDefaultVersion("/city", "https://example.com/tools.git")
		if err != nil || got != "sha:deadbeef" {
			t.Fatalf("got %q, %v; want sha:deadbeef", got, err)
		}
	})

	t.Run("tag listing failure", func(t *testing.T) {
		var c calls
		_, err := newDeps(&c, nil, "", errTags).ResolveRemoteDefaultVersion("/city", "https://example.com/tools.git")
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
			_, err := d.ResolveRemoteDefaultVersion("/city", tc.source)
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
