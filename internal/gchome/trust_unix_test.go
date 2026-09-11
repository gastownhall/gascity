//go:build (linux && !android) || (darwin && !ios)

package gchome

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testEUID = uint32(1000)

func TestInspectTrustedProductUsagePathPredicate(t *testing.T) {
	home := "/safe/users/alice/.gc"
	root := home + "/product-usage"
	base := map[string]componentInfo{
		"/":                 directoryInfo(0, 0o755),
		"/safe":             directoryInfo(0, 0o755),
		"/safe/users":       directoryInfo(0, 0o755),
		"/safe/users/alice": directoryInfo(testEUID, 0o700),
		home:                directoryInfo(testEUID, 0o700),
		root:                directoryInfo(testEUID, 0o700),
	}
	tests := []struct {
		name       string
		mutate     func(map[string]componentInfo)
		remove     []string
		statErr    map[string]error
		wantCreate bool
		wantErr    bool
	}{
		{name: "trusted existing tree"},
		{name: "missing product root", remove: []string{root}, wantCreate: true},
		{name: "missing home suffix", remove: []string{home, root}, wantCreate: true},
		{name: "missing multiple components", remove: []string{"/safe/users/alice", home, root}, wantCreate: true},
		{name: "root-owned ancestors accepted", mutate: func(tree map[string]componentInfo) {
			tree["/safe/users/alice"] = directoryInfo(0, 0o755)
		}},
		{name: "effective-UID ancestor accepted", mutate: func(tree map[string]componentInfo) {
			tree["/safe"] = directoryInfo(testEUID, 0o755)
		}},
		{name: "group-writable parent rejected", mutate: func(tree map[string]componentInfo) {
			tree["/safe"] = directoryInfo(0, 0o775)
		}, wantErr: true},
		{name: "world-writable non-sticky parent rejected", mutate: func(tree map[string]componentInfo) {
			tree["/safe"] = directoryInfo(0, 0o777)
		}, wantErr: true},
		{name: "foreign-owned parent rejected", mutate: func(tree map[string]componentInfo) {
			tree["/safe"] = directoryInfo(2000, 0o755)
		}, wantErr: true},
		{name: "foreign-owned home rejected", mutate: func(tree map[string]componentInfo) {
			tree[home] = directoryInfo(2000, 0o700)
		}, wantErr: true},
		{name: "root-owned home rejected", mutate: func(tree map[string]componentInfo) {
			tree[home] = directoryInfo(0, 0o700)
		}, wantErr: true},
		{name: "foreign-owned product root rejected", mutate: func(tree map[string]componentInfo) {
			tree[root] = directoryInfo(2000, 0o700)
		}, wantErr: true},
		{name: "home group bits rejected", mutate: func(tree map[string]componentInfo) {
			tree[home] = directoryInfo(testEUID, 0o750)
		}, wantErr: true},
		{name: "setgid home rejected", mutate: func(tree map[string]componentInfo) {
			tree[home] = directoryInfo(testEUID, fs.ModeSetgid|0o700)
		}, wantErr: true},
		{name: "product root other bits rejected", mutate: func(tree map[string]componentInfo) {
			tree[root] = directoryInfo(testEUID, 0o701)
		}, wantErr: true},
		{name: "default ACL reflected in mode rejected", mutate: func(tree map[string]componentInfo) {
			tree[root] = directoryInfo(testEUID, 0o770)
		}, wantErr: true},
		{name: "symlink ancestor rejected", mutate: func(tree map[string]componentInfo) {
			tree["/safe/users"] = componentInfo{uid: 0, mode: fs.ModeSymlink | 0o777}
		}, wantErr: true},
		{name: "symlink home rejected", mutate: func(tree map[string]componentInfo) {
			tree[home] = componentInfo{uid: testEUID, mode: fs.ModeSymlink | 0o700}
		}, wantErr: true},
		{name: "non-directory component rejected", mutate: func(tree map[string]componentInfo) {
			tree["/safe/users"] = componentInfo{uid: 0, mode: 0o600}
		}, wantErr: true},
		{name: "unstatable component rejected", statErr: map[string]error{"/safe/users": fs.ErrPermission}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree := cloneComponentTree(base)
			if test.mutate != nil {
				test.mutate(tree)
			}
			for _, path := range test.remove {
				delete(tree, path)
			}
			needsCreation, err := inspectTrustedProductUsagePathWith(home, root, testEUID, fakeLstat(tree, test.statErr))
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && needsCreation != test.wantCreate {
				t.Fatalf("needsCreation = %v, want %v", needsCreation, test.wantCreate)
			}
		})
	}
}

func TestRootOwnedStickyExceptionRequiresLaterPrivateHome(t *testing.T) {
	home := "/tmp/alice-gc"
	root := home + "/product-usage"
	base := map[string]componentInfo{
		"/":    directoryInfo(0, 0o755),
		"/tmp": directoryInfo(0, fs.ModeSticky|0o777),
		home:   directoryInfo(testEUID, 0o700),
		root:   directoryInfo(testEUID, 0o700),
	}
	if _, err := inspectTrustedProductUsagePathWith(home, root, testEUID, fakeLstat(base, nil)); err != nil {
		t.Fatalf("root-owned sticky ancestor above private home rejected: %v", err)
	}
	missingRoot := cloneComponentTree(base)
	delete(missingRoot, root)
	if needsCreation, err := inspectTrustedProductUsagePathWith(home, root, testEUID, fakeLstat(missingRoot, nil)); err != nil || !needsCreation {
		t.Fatalf("missing product root below sticky ancestor and private home = create:%v err:%v, want create:true", needsCreation, err)
	}

	withoutHome := cloneComponentTree(base)
	delete(withoutHome, home)
	delete(withoutHome, root)
	if _, err := inspectTrustedProductUsagePathWith(home, root, testEUID, fakeLstat(withoutHome, nil)); err == nil {
		t.Fatal("root-owned sticky ancestor accepted without a later existing private home")
	}

	nonRootSticky := cloneComponentTree(base)
	nonRootSticky["/tmp"] = directoryInfo(testEUID, fs.ModeSticky|0o777)
	if _, err := inspectTrustedProductUsagePathWith(home, root, testEUID, fakeLstat(nonRootSticky, nil)); err == nil {
		t.Fatal("effective-UID-owned world-writable sticky ancestor accepted; exception must be UID 0 only")
	}
}

func TestRootOwnedStickyExceptionAllowsMissingHomeBelowExistingPrivateAncestor(t *testing.T) {
	home := "/tmp/alice-private/missing-gc-home"
	root := home + "/product-usage"
	withPrivateAncestor := map[string]componentInfo{
		"/":                  directoryInfo(0, 0o755),
		"/tmp":               directoryInfo(0, fs.ModeSticky|0o777),
		"/tmp/alice-private": directoryInfo(testEUID, 0o700),
	}
	needsCreation, err := inspectTrustedProductUsagePathWith(home, root, testEUID, fakeLstat(withPrivateAncestor, nil))
	if err != nil || !needsCreation {
		t.Fatalf("private ancestor then missing home = create:%v err:%v, want create:true", needsCreation, err)
	}

	withoutPrivateAncestor := map[string]componentInfo{
		"/":    directoryInfo(0, 0o755),
		"/tmp": directoryInfo(0, fs.ModeSticky|0o777),
	}
	if _, err := inspectTrustedProductUsagePathWith(home, root, testEUID, fakeLstat(withoutPrivateAncestor, nil)); err == nil {
		t.Fatal("sticky ancestor followed by missing private boundary unexpectedly accepted")
	}
}

func TestMissingSuffixStopsAtNearestTrustedExistingAncestor(t *testing.T) {
	home := "/safe/missing/home"
	root := home + "/product-usage"
	visited := []string{}
	lstat := func(path string) (componentInfo, error) {
		visited = append(visited, path)
		switch path {
		case "/":
			return directoryInfo(0, 0o755), nil
		case "/safe":
			return directoryInfo(0, 0o755), nil
		case "/safe/missing":
			return componentInfo{}, fs.ErrNotExist
		default:
			t.Fatalf("inspector continued beyond first missing component to %q", path)
			return componentInfo{}, fs.ErrInvalid
		}
	}
	needsCreation, err := inspectTrustedProductUsagePathWith(home, root, testEUID, lstat)
	if err != nil || !needsCreation {
		t.Fatalf("inspection = create:%v err:%v, want create:true", needsCreation, err)
	}
	want := []string{"/", "/safe", "/safe/missing"}
	if strings.Join(visited, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("visited = %q, want %q", visited, want)
	}
}

func TestMissingLexicalRootFailsClosedWithoutTrustedAncestor(t *testing.T) {
	home := "/missing/home"
	root := home + "/product-usage"
	visited := []string{}
	lstat := func(path string) (componentInfo, error) {
		visited = append(visited, path)
		if path != "/" {
			t.Fatalf("inspector continued past missing lexical root to %q", path)
		}
		return componentInfo{}, fs.ErrNotExist
	}

	needsCreation, err := inspectTrustedProductUsagePathWith(home, root, testEUID, lstat)
	if err == nil {
		t.Fatal("missing lexical root unexpectedly accepted without a trusted existing ancestor")
	}
	if needsCreation {
		t.Fatal("missing lexical root reported creatable without a trusted existing ancestor")
	}
	want := []string{"/"}
	if strings.Join(visited, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("visited = %q, want %q", visited, want)
	}
}

func TestInspectProductUsageHomeIsReadOnlyForMissingRoot(t *testing.T) {
	home := trustedTemporaryDirectory(t)
	root := filepath.Join(home, "product-usage")
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("precondition Lstat(%q) = %v, want not exist", root, err)
	}
	resolved := ResolvedHome{path: home, provenance: ProvenanceExplicit}
	got, err := InspectProductUsageHome(resolved)
	if err != nil {
		t.Fatalf("InspectProductUsageHome: %v", err)
	}
	if got.Root() != root || !got.NeedsCreation() {
		t.Fatalf("inspection = root:%q create:%v, want root:%q create:true", got.Root(), got.NeedsCreation(), root)
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("inspection created %q or changed error: %v", root, err)
	}
}

func TestInspectProductUsageHomeRejectsSymlinkWithoutResolvingIt(t *testing.T) {
	parent := trustedTemporaryDirectory(t)
	realHome := filepath.Join(parent, "real")
	linkHome := filepath.Join(parent, "link")
	if err := os.Mkdir(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolved := ResolvedHome{path: linkHome, provenance: ProvenanceExplicit}
	if _, err := InspectProductUsageHome(resolved); err == nil || !strings.Contains(err.Error(), linkHome) {
		t.Fatalf("InspectProductUsageHome(symlink) error = %v, want rejection naming lexical link", err)
	}
	if _, err := os.Lstat(filepath.Join(realHome, "product-usage")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("inspection followed symlink or created target root: %v", err)
	}
}

func trustedTemporaryDirectory(t *testing.T) string {
	t.Helper()
	tempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Skipf("cannot resolve system temporary root for trust smoke test: %v", err)
	}
	directory, err := os.MkdirTemp(tempRoot, "gchome-trust-test-*")
	if err != nil {
		t.Skipf("cannot create trust smoke-test directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func directoryInfo(uid uint32, permissions fs.FileMode) componentInfo {
	return componentInfo{uid: uid, mode: fs.ModeDir | permissions}
}

func cloneComponentTree(source map[string]componentInfo) map[string]componentInfo {
	clone := make(map[string]componentInfo, len(source))
	for path, info := range source {
		clone[path] = info
	}
	return clone
}

func fakeLstat(tree map[string]componentInfo, failures map[string]error) componentLstat {
	return func(path string) (componentInfo, error) {
		if err := failures[path]; err != nil {
			return componentInfo{}, err
		}
		info, ok := tree[path]
		if !ok {
			return componentInfo{}, fs.ErrNotExist
		}
		return info, nil
	}
}
