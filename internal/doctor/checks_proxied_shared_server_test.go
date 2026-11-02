package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func sharedServerCheckScope(t *testing.T, cityPath, rel, config string) string {
	t.Helper()
	root := filepath.Join(cityPath, rel)
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(root, ".beads", "config.yaml"), []byte(config), 0o644); err != nil { //nolint:gosec // fixture
			t.Fatal(err)
		}
	}
	return root
}

func newTestSharedServerCheck(cityPath string, userOn bool, roots ...string) *ProxiedSharedServerCheck {
	c := NewProxiedSharedServerCheck(cityPath, roots, nil)
	c.sharedRoot = func() string { return filepath.Join(cityPath, "no-shared-server") }
	c.userMode = func() (bool, string) {
		if userOn {
			return true, "/home/op/.beads/config.yaml"
		}
		return false, ""
	}
	return c
}

func TestProxiedSharedServerCheckIsOnlyRegisteredForProxiedScopes(t *testing.T) {
	if c := NewProxiedSharedServerCheck(t.TempDir(), nil, nil); c != nil {
		t.Fatalf("check registered for a city with no gc-owned proxied scope: %+v", c)
	}
}

func TestProxiedSharedServerCheck(t *testing.T) {
	const pinnedOff = "dolt:\n  shared-server: false\n"

	t.Run("user-level on, scopes pinned: ok", func(t *testing.T) {
		city := t.TempDir()
		c := newTestSharedServerCheck(city, true,
			sharedServerCheckScope(t, city, ".", pinnedOff),
			sharedServerCheckScope(t, city, "rigs/a", pinnedOff))
		r := c.Run(&CheckContext{CityPath: city})
		if r.Status != StatusOK || !strings.Contains(r.Message, "pin it off") {
			t.Fatalf("result = %+v, want OK naming the pin", r)
		}
	})

	t.Run("user-level on, a scope unpinned: warning, fixable", func(t *testing.T) {
		city := t.TempDir()
		cityRoot := sharedServerCheckScope(t, city, ".", pinnedOff)
		rig := sharedServerCheckScope(t, city, "rigs/a", "# bd init template\n")
		c := newTestSharedServerCheck(city, true, cityRoot, rig)
		r := c.Run(&CheckContext{CityPath: city})
		if r.Status != StatusWarning || !strings.Contains(r.Message, "rigs/a") {
			t.Fatalf("result = %+v, want a warning naming rigs/a", r)
		}
		if !c.CanFix() {
			t.Fatal("CanFix = false")
		}
		if err := c.Fix(&CheckContext{CityPath: city}); err != nil {
			t.Fatal(err)
		}
		if pin, err := contract.ReadSharedServerPin(fsys.OSFS{}, filepath.Join(rig, ".beads", "config.yaml")); err != nil || pin != contract.SharedServerPinnedOff {
			t.Fatalf("pin after fix = %v, %v", pin, err)
		}
		if r := c.Run(&CheckContext{CityPath: city}); r.Status != StatusOK {
			t.Fatalf("after fix = %+v, want OK", r)
		}
	})

	t.Run("user-level off, a scope unpinned: ok", func(t *testing.T) {
		city := t.TempDir()
		c := newTestSharedServerCheck(city, false, sharedServerCheckScope(t, city, ".", ""))
		if r := c.Run(&CheckContext{CityPath: city}); r.Status != StatusOK {
			t.Fatalf("result = %+v, want OK: nothing relocates the store", r)
		}
	})

	t.Run("scope bound on: error regardless of user level", func(t *testing.T) {
		city := t.TempDir()
		c := newTestSharedServerCheck(city, false, sharedServerCheckScope(t, city, ".", "dolt.shared-server: true\n"))
		r := c.Run(&CheckContext{CityPath: city})
		if r.Status != StatusError || !strings.Contains(r.Message, "shared server") {
			t.Fatalf("result = %+v, want an error naming the shared server", r)
		}
	})
}

// UserLevelBdSharedServerMode must follow bd's precedence without ever
// touching the real home directory.
func TestUserLevelBdSharedServerMode(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
	t.Setenv("BD_DOLT_SHARED_SERVER", "")

	if on, _ := UserLevelBdSharedServerMode(); on {
		t.Fatal("reported on with no config at all")
	}

	legacy := filepath.Join(home, ".beads", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("dolt:\n    shared-server: true\n"), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	if on, src := UserLevelBdSharedServerMode(); !on || src != legacy {
		t.Fatalf("legacy config: on=%v src=%q, want on from %s", on, src, legacy)
	}

	// ~/.config/bd outranks the legacy file. (Not XDG_CONFIG_HOME/bd: on macOS
	// os.UserConfigDir ignores XDG, so only the literal ~/.config path is
	// portable.)
	userCfg := filepath.Join(home, ".config", "bd", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(userCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userCfg, []byte("dolt.shared-server: false\n"), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	if on, src := UserLevelBdSharedServerMode(); on || src != userCfg {
		t.Fatalf("user config off: on=%v src=%q, want off from %s", on, src, userCfg)
	}

	// The env outranks both files.
	t.Setenv("BD_DOLT_SHARED_SERVER", "true")
	if on, src := UserLevelBdSharedServerMode(); !on || src != "BD_DOLT_SHARED_SERVER" {
		t.Fatalf("BD_ env: on=%v src=%q", on, src)
	}
	t.Setenv("BD_DOLT_SHARED_SERVER", "")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	if on, src := UserLevelBdSharedServerMode(); !on || src != "BEADS_DOLT_SHARED_SERVER" {
		t.Fatalf("BEADS_ env: on=%v src=%q", on, src)
	}
}

// Beads a scope wrote while bound to the shared server stay there after the
// pin flips off; doctor keeps saying so for as long as the database exists.
func TestProxiedSharedServerCheckWarnsAboutStrandedSharedServerDatabase(t *testing.T) {
	city := t.TempDir()
	root := sharedServerCheckScope(t, city, ".", "dolt:\n  shared-server: false\n")
	if err := os.WriteFile(filepath.Join(root, ".beads", "metadata.json"), []byte(`{"dolt_database":"hq"}`), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	shared := filepath.Join(city, "shared")
	c := newTestSharedServerCheck(city, false, root)
	c.sharedRoot = func() string { return shared }
	if r := c.Run(&CheckContext{CityPath: city}); r.Status != StatusOK {
		t.Fatalf("no shared database yet: %+v, want OK", r)
	}
	if err := os.MkdirAll(filepath.Join(shared, "dolt", "hq"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := c.Run(&CheckContext{CityPath: city})
	if r.Status != StatusWarning || !strings.Contains(r.Message, "stranded") || !strings.Contains(r.Message, filepath.Join(shared, "dolt", "hq")) {
		t.Fatalf("result = %+v, want a stranded-data warning naming the database", r)
	}
}

// A journaled scope bd has not materialized yet is neither reported unpinned
// nor touched by --fix (which used to fail writing into a missing .beads).
func TestProxiedSharedServerCheckSkipsUnmaterializedScopes(t *testing.T) {
	city := t.TempDir()
	cityRoot := sharedServerCheckScope(t, city, ".", "dolt:\n  shared-server: false\n")
	fresh := filepath.Join(city, "rigs", "fresh")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	c := newTestSharedServerCheck(city, true, cityRoot, fresh)
	if r := c.Run(&CheckContext{CityPath: city}); r.Status != StatusOK {
		t.Fatalf("result = %+v, want OK", r)
	}
	if err := c.Fix(&CheckContext{CityPath: city}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("Fix created %s/.beads (stat err %v)", fresh, err)
	}
}

// Scopes whose ownership could not be classified are reported, not dropped.
func TestProxiedSharedServerCheckReportsClassificationErrors(t *testing.T) {
	city := t.TempDir()
	c := NewProxiedSharedServerCheck(city, nil, []error{os.ErrPermission})
	if c == nil {
		t.Fatal("check not registered for a classification failure")
	}
	c.userMode = func() (bool, string) { return false, "" }
	c.sharedRoot = func() string { return "" }
	if r := c.Run(&CheckContext{CityPath: city}); r.Status != StatusWarning {
		t.Fatalf("result = %+v, want a warning", r)
	}
}
