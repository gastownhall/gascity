package productmetrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestCurrentReleaseIdentityIgnoresEnvAndDerivesDevelopmentWithoutReleaseTag(t *testing.T) {
	for _, env := range []string{
		"GC_PRODUCT_METRICS_ENDPOINT",
		"GC_PRODUCT_METRICS_BUILD_KIND",
		"GC_PRODUCT_METRICS_RELEASE_VERSION",
		"GC_PRODUCT_METRICS_EPOCH",
		"GC_PRODUCT_METRICS_ROLLOUT",
	} {
		t.Setenv(env, "official-default-on-https://invalid.example-99")
	}
	got := CurrentReleaseIdentity()
	// The test binary carries no injected release tag, so it derives the
	// development identity; environment variables cannot promote it, and the
	// redirect-security core matches the compiled constants.
	if got.BuildKind() != BuildDevelopment {
		t.Errorf("BuildKind = %v, want development", got.BuildKind())
	}
	if got.ReleaseVersion() != developmentReleaseVersion {
		t.Errorf("ReleaseVersion = %q, want %q", got.ReleaseVersion(), developmentReleaseVersion)
	}
	if got.Endpoint() != compiledEndpoint {
		t.Errorf("Endpoint = %q, want compiled %q", got.Endpoint(), compiledEndpoint)
	}
	if got.PrivacyURL() != compiledPrivacyURL {
		t.Errorf("PrivacyURL = %q, want compiled %q", got.PrivacyURL(), compiledPrivacyURL)
	}
	if got.MetricsEpoch() != compiledMetricsEpoch {
		t.Errorf("MetricsEpoch = %d, want compiled %d", got.MetricsEpoch(), compiledMetricsEpoch)
	}
	if got.Rollout() != compiledRollout {
		t.Errorf("Rollout = %v, want compiled %v", got.Rollout(), compiledRollout)
	}
}

func TestClassifyBuildTaxonomy(t *testing.T) {
	cases := []struct {
		name        string
		tag         string
		dirty       bool
		wantKind    BuildKind
		wantVersion string
	}{
		{"empty is development", "", false, BuildDevelopment, developmentReleaseVersion},
		{"stable clean semver is release", "1.4.2", false, BuildRelease, "1.4.2"},
		{"prerelease is canary", "1.4.2-canary-abc1234", false, BuildCanary, "1.4.2-canary-abc1234"},
		{"rc prerelease is canary", "1.4.2-rc1", false, BuildCanary, "1.4.2-rc1"},
		{"dirty release tag falls back to development", "1.4.2", true, BuildDevelopment, developmentReleaseVersion},
		{"leading-v tag is not canonical semver", "v1.4.2", false, BuildDevelopment, developmentReleaseVersion},
		{"non-semver garbage is development", "not-a-version", false, BuildDevelopment, developmentReleaseVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotKind, gotVersion := classifyBuild(tc.tag, tc.dirty)
			if gotKind != tc.wantKind || gotVersion != tc.wantVersion {
				t.Errorf("classifyBuild(%q, %v) = (%v, %q), want (%v, %q)",
					tc.tag, tc.dirty, gotKind, gotVersion, tc.wantKind, tc.wantVersion)
			}
		})
	}
}

func TestCompiledReleaseInputsAreConstantsNotLinkerVariables(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "release.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The redirect-security identity fields must stay const so no ordinary
	// -ldflags -X can promote a build (change endpoint/rollout/epoch/privacy).
	// Only compiledReleaseTag — a reporting label that cannot redirect data or
	// bypass consent — is a deliberately injectable var.
	wantConst := map[string]bool{
		"compiledEndpoint":     false,
		"compiledPrivacyURL":   false,
		"compiledMetricsEpoch": false,
		"compiledRollout":      false,
	}
	tagIsVar := false
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range values.Names {
				if name.Name == "compiledReleaseTag" {
					tagIsVar = general.Tok == token.VAR
				}
				if _, tracked := wantConst[name.Name]; !tracked {
					continue
				}
				if general.Tok != token.CONST {
					t.Errorf("%s is %s, allowing ordinary -X promotion; want const", name.Name, general.Tok)
				}
				wantConst[name.Name] = true
			}
		}
	}
	for name, found := range wantConst {
		if !found {
			t.Errorf("compiled release input %s not found", name)
		}
	}
	if !tagIsVar {
		t.Error("compiledReleaseTag must be a var (the single deliberate linker-injectable input)")
	}
}
