package docsync

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/worker/builtin"
)

// harnessRecipesDoc is the user guide that documents each built-in harness.
const harnessRecipesDoc = "docs/guides/harness-recipes.md"

// TestBuiltinProviderEnvDefaultsAreDocumented guards built-in provider env
// defaults. An env default never appears in the launch command line, so a
// preset may not inject a variable that the harness guide never names: users
// need to know the default exists and how to override or withhold it.
func TestBuiltinProviderEnvDefaultsAreDocumented(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), harnessRecipesDoc))
	if err != nil {
		t.Fatalf("reading %s: %v", harnessRecipesDoc, err)
	}
	doc := string(data)

	providers := builtin.BuiltinProviders()
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		keys := make([]string, 0, len(providers[name].Env))
		for key := range providers[name].Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !strings.Contains(doc, key) {
				t.Errorf("built-in provider %q sets env %s by default, but %s never mentions it; document the default and how to override it", name, key, harnessRecipesDoc)
			}
		}
	}
}
