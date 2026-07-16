package productmetrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"
)

func TestCurrentReleaseIdentityIsInertAndRuntimeUnpromotable(t *testing.T) {
	want := ReleaseIdentity{}
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
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CurrentReleaseIdentity() = %#v, want inert zero identity %#v", got, want)
	}
	if got.BuildKind() != BuildDevelopment {
		t.Errorf("BuildKind = %v, want development", got.BuildKind())
	}
	if got.ReleaseVersion() != "" {
		t.Errorf("ReleaseVersion = %q, want empty", got.ReleaseVersion())
	}
	if got.Endpoint() != "" {
		t.Errorf("Endpoint = %q, want empty", got.Endpoint())
	}
	if got.MetricsEpoch() != 0 {
		t.Errorf("MetricsEpoch = %d, want zero", got.MetricsEpoch())
	}
	if got.Rollout() != RolloutDefaultOff {
		t.Errorf("Rollout = %v, want default-off", got.Rollout())
	}
}

func TestCompiledReleaseInputsAreConstantsNotLinkerVariables(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "release.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"compiledBuildKind":      false,
		"compiledReleaseVersion": false,
		"compiledEndpoint":       false,
		"compiledMetricsEpoch":   false,
		"compiledRollout":        false,
	}
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
				if _, tracked := want[name.Name]; !tracked {
					continue
				}
				if general.Tok != token.CONST {
					t.Errorf("%s is %s, allowing ordinary -X promotion; want const", name.Name, general.Tok)
				}
				want[name.Name] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("compiled release input %s not found", name)
		}
	}
}
