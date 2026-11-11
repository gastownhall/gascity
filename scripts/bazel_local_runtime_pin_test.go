package scripts_test

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const localRuntimePinPlaces = "update MODULE.bazel, .githooks/lib/local-runtime.sh, and engdocs/bazel-quickstart.md together"

// checkLocalRuntimePin binds the probe's actual output to the retained ICU
// libraries in the pinned sysroot. It intentionally does not parse the pin at
// hook runtime or depend on a worker-env fingerprint.
func checkLocalRuntimePin(module, sonames, quickstart string, hooks map[string]string) error {
	bad := func(reason string) error { return fmt.Errorf("%s: %s", reason, localRuntimePinPlaces) }
	call, err := moduleCall(module, "deb_sysroot", `name = "cc_sysroot_noble_amd64"`)
	if err != nil {
		return bad(err.Error())
	}
	pkgBody := regexp.MustCompile(`(?s)packages = \{(.*?)\}`).FindStringSubmatch(call)
	if pkgBody == nil {
		return bad("sysroot package pins are missing")
	}
	var majors []string
	for path := range quotedPairs(pkgBody[1]) {
		if m := regexp.MustCompile(`libicu(\d+)_`).FindStringSubmatch(path); m != nil {
			majors = append(majors, m[1])
		}
	}
	if len(majors) != 1 {
		return bad("want one libicuN runtime package pin")
	}
	removeBody := regexp.MustCompile(`(?s)remove = \[(.*?)\]`).FindStringSubmatch(call)
	if removeBody == nil {
		return bad("sysroot remove list is missing")
	}
	var globs []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(removeBody[1], -1) {
		globs = append(globs, m[1])
	}
	var want []string
	for _, base := range []string{"data", "i18n", "io", "test", "tu", "uc"} {
		name := "libicu" + base + ".so." + majors[0]
		removed := false
		for _, glob := range globs {
			match, err := filepath.Match(glob, "usr/lib/x86_64-linux-gnu/"+name)
			if err != nil {
				return bad("invalid sysroot remove glob: " + glob)
			}
			removed = removed || match
		}
		if !removed {
			want = append(want, name)
		}
	}
	got := strings.Fields(sonames)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return bad(fmt.Sprintf("probe sonames %v differ from retained sysroot libraries %v", got, want))
	}
	for path, body := range hooks {
		if m := regexp.MustCompile(`libicu\w*\.so\.\d+`).FindString(body); m != "" {
			return bad("second hardcoded soname " + m + " in " + path)
		}
	}
	m := regexp.MustCompile("ICU (\\d+)\\s+\\(`libicu(\\d+)`\\)").FindStringSubmatch(quickstart)
	if m == nil || m[1] != majors[0] || m[2] != majors[0] {
		return bad("quickstart runtime requirement differs from the ICU pin")
	}
	return nil
}

func TestBazelLocalRuntimePinMatchesProbe(t *testing.T) {
	root := repoRoot(t)
	sonames := runLocalRuntimeShell(t, "", "local_runtime_sonames\n")
	hooks := map[string]string{}
	if err := filepath.WalkDir(filepath.Join(root, ".githooks"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hooks[rel] = readFile(t, root, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := checkLocalRuntimePin(readFile(t, root, "MODULE.bazel"), sonames, readFile(t, root, "engdocs/bazel-quickstart.md"), hooks); err != nil {
		t.Fatal(err)
	}
}

func TestBazelLocalRuntimePinGuardRejectsDrift(t *testing.T) {
	module := `deb_sysroot(
    name = "cc_sysroot_noble_amd64",
    packages = {
        "pool/main/i/icu/libicu74_74.2-1ubuntu3.1_amd64.deb": "pinned-sha",
    },
    remove = [
        "usr/lib/x86_64-linux-gnu/libicu*.a",
        "usr/lib/x86_64-linux-gnu/libicuio.*",
        "usr/lib/x86_64-linux-gnu/libicutest.*",
        "usr/lib/x86_64-linux-gnu/libicutu.*",
    ],
)
`
	sonames, docs := "libicui18n.so.74 libicuuc.so.74 libicudata.so.74\n", "loads ICU 74 (`libicu74`) at run time"
	if err := checkLocalRuntimePin(module, sonames, docs, nil); err != nil {
		t.Fatalf("good pin fixture: %v", err)
	}
	for _, tc := range []struct {
		name, module, sonames, docs string
		hooks                       map[string]string
	}{
		{name: "major bump leaves stale probe", module: strings.Replace(module, "libicu74_", "libicu75_", 1)},
		{name: "removing an extra runtime lib", module: strings.Replace(module, `"usr/lib/x86_64-linux-gnu/libicu*.a",`, `"usr/lib/x86_64-linux-gnu/libicu*.a", "usr/lib/x86_64-linux-gnu/libicuuc.*",`, 1)},
		{name: "retaining io requires a new soname", module: strings.Replace(module, `"usr/lib/x86_64-linux-gnu/libicuio.*",`, "", 1)},
		{name: "retaining test requires a new soname", module: strings.Replace(module, `"usr/lib/x86_64-linux-gnu/libicutest.*",`, "", 1)},
		{name: "retaining tu requires a new soname", module: strings.Replace(module, `"usr/lib/x86_64-linux-gnu/libicutu.*",`, "", 1)},
		{name: "a second literal can drift", hooks: map[string]string{".githooks/pre-commit": "missing=libicuuc.so.74"}},
		{name: "library literals are also forbidden", hooks: map[string]string{localRuntimeLib: "printf 'libicuuc.so.74'"}},
		{name: "doc major changed", docs: strings.Replace(docs, "ICU 74", "ICU 77", 1)},
		{name: "doc package changed", docs: strings.Replace(docs, "libicu74", "libicu77", 1)},
		{name: "probe duplicates a name", sonames: sonames + "libicuuc.so.74"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s, d := module, sonames, docs
			if tc.module != "" {
				m = tc.module
			}
			if tc.sonames != "" {
				s = tc.sonames
			}
			if tc.docs != "" {
				d = tc.docs
			}
			if err := checkLocalRuntimePin(m, s, d, tc.hooks); err == nil || !strings.Contains(err.Error(), localRuntimePinPlaces) {
				t.Fatalf("drift error = %v, want update locations", err)
			}
		})
	}
}
