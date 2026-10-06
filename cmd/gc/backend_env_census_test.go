package main

import (
	"encoding/json"
	"go/ast"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// removedBackendVocabulary names the storage backends whose vocabulary was
// lifted out of OSS. A backend gc does not implement has no business owning an
// environment variable here: gc withholds the whole projected namespace for a
// scope bound to one and lets the linked beads library run its own credential
// ladder (applyCompleteNonDoltStorageBindingEnv).
//
// The list is spelled out rather than derived because that is the fact under
// test. Deriving it from the registry would make the test vacuous the moment
// somebody re-registered a name.
var removedBackendVocabulary = []string{"postgres", "postgresql", "pgsql"}

// TestOSSProjectsNoUnregisteredBackendEnv is the standing proof that OSS never
// projects credentials for a backend it does not implement.
//
// It replaces what internal/pgauth's own census provided. That test held a
// weaker line — every Postgres env read had to live inside the resolver
// package — because OSS still owned a resolver. OSS owns none now, so the line
// moves: the names must not appear at all.
//
// Three halves, because a credential can leak through three different doors:
// the registry can readmit the backend, a source file can name its environment
// variables, and the projector can write them at runtime.
func TestOSSProjectsNoUnregisteredBackendEnv(t *testing.T) {
	t.Run("registry", testRemovedBackendsAreNotRegistered)
	t.Run("source", testNoRemovedBackendEnvNamesInSource)
	t.Run("projection", testUnregisteredBackendScopeProjectsNoBackendEnv)
}

// testRemovedBackendsAreNotRegistered is the registry half: an assembly that
// re-registers a removed name re-opens every refusal path that keys on
// RecognizeBackend, so the census below would stop describing the binary.
func testRemovedBackendsAreNotRegistered(t *testing.T) {
	registered, err := contract.RegisteredBackends()
	if err != nil {
		t.Fatalf("RegisteredBackends: %v", err)
	}
	for _, name := range registered {
		for _, removed := range removedBackendVocabulary {
			if strings.EqualFold(name, removed) {
				t.Errorf("backend %q is registered by this build; the OSS registry must carry only backends OSS implements (registered: %s)",
					name, strings.Join(registered, ", "))
			}
		}
	}
	for _, removed := range removedBackendVocabulary {
		if err := contract.RecognizeBackend(removed); err == nil {
			t.Errorf("RecognizeBackend(%q) = nil; an unimplemented backend must be refused by name", removed)
		}
	}
}

// testNoRemovedBackendEnvNamesInSource is the source half: an env key naming a
// backend OSS does not implement is a credential channel for a backend nothing
// here can serve, whether or not anything writes to it today.
func testNoRemovedBackendEnvNamesInSource(t *testing.T) {
	root := moduleRoot(t)
	var violations []string
	for _, rel := range moduleGoFiles(t, root) {
		if strings.HasPrefix(rel, "scripts/") {
			continue
		}
		file := parseModuleFile(t, root, rel)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind.String() != "STRING" {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if name, ok := removedBackendEnvName(value); ok {
				violations = append(violations, rel+": "+name)
			}
			return true
		})
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("OSS names environment variables for backends it does not implement:\n  %s\n\ngc withholds the whole projected namespace for a scope bound to such a backend; the linked beads library reads its own configuration and runs its own credential ladder.",
			strings.Join(violations, "\n  "))
	}
}

// removedBackendEnvName reports whether value is an environment-variable name
// in one of gc's three projected namespaces whose vocabulary segment names a
// backend OSS does not implement.
func removedBackendEnvName(value string) (string, bool) {
	upper := strings.ToUpper(value)
	if upper != value || !strings.Contains(value, "_") {
		return "", false
	}
	var segments []string
	switch {
	case strings.HasPrefix(value, "BEADS_"):
		segments = strings.Split(strings.TrimPrefix(value, "BEADS_"), "_")
	case strings.HasPrefix(value, "GC_"):
		segments = strings.Split(strings.TrimPrefix(value, "GC_"), "_")
	case strings.HasPrefix(value, "BD_"):
		segments = strings.Split(strings.TrimPrefix(value, "BD_"), "_")
	default:
		return "", false
	}
	for _, segment := range segments {
		for _, removed := range removedBackendVocabulary {
			if strings.EqualFold(segment, removed) {
				return value, true
			}
		}
	}
	return "", false
}

// testUnregisteredBackendScopeProjectsNoBackendEnv is the runtime half. A city
// whose metadata names a backend this build does not register must be refused
// by name, and the env handed back on that path must carry no connection or
// credential value for it — the refusal and the empty env are one guarantee,
// not two.
func testUnregisteredBackendScopeProjectsNoBackendEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	cityPath := t.TempDir()
	writeCityFile(t, cityPath, "city.toml", "[workspace]\nname = \"demo\"\n")
	writeScopeMetadata(t, cityPath, map[string]string{"database": "beads", "backend": "postgres"})
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	env, err := bdRuntimeEnvWithErrorNoRecovery(cityPath)
	if err == nil {
		t.Fatal("bdRuntimeEnvWithErrorNoRecovery accepted a scope whose backend this build does not register")
	}
	registered, regErr := contract.RegisteredBackends()
	if regErr != nil {
		t.Fatalf("RegisteredBackends: %v", regErr)
	}
	for _, want := range append([]string{`"postgres"`}, strings.Join(registered, ", ")) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q; an operator must be told the name and what this build does register", err, want)
		}
	}
	for key, value := range env {
		if _, bad := removedBackendEnvName(key); bad && strings.TrimSpace(value) != "" {
			t.Errorf("projected %s=%q for a backend this build does not implement", key, value)
		}
	}
}

func writeCityFile(t *testing.T, cityPath, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityPath, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func writeScopeMetadata(t *testing.T, scopeRoot string, fields map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(scopeMetadataJSONPath(scopeRoot), data, 0o600); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}
