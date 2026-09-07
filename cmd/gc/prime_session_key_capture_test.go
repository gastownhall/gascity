package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestPersistPrimeClaudeHookSessionKeyGuards(t *testing.T) {
	for _, name := range []string{"existing key", "GC id collision", "foreign bead", "custom Claude provider"} {
		t.Run(name, func(t *testing.T) {
			dir, id := setupPrimeHookProviderSessionKeyTest(t, "claude", "[providers.claude]\nbase = \"builtin:claude\"")
			store, err := openCityStoreAt(dir)
			if err != nil {
				t.Fatal(err)
			}
			input, want := "claude-provider-session", ""
			switch name {
			case "existing key":
				want = "original-session"
				err = store.SetMetadata(id, "session_key", want)
			case "GC id collision":
				input = id
			case "foreign bead":
				var foreign beads.Bead
				foreign, err = store.Create(beads.Bead{Title: "ordinary work", Type: "task", Metadata: map[string]string{"provider": "claude"}})
				id = foreign.ID
				t.Setenv("GC_SESSION_ID", id)
			case "custom Claude provider":
				want = input
				err = store.SetMetadataBatch(id, map[string]string{"provider": "custom-agent", "provider_kind": "custom-agent", "builtin_ancestor": "claude"})
			}
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			persistPrimeHookProviderSessionKey(input, &stderr)
			updatedStore, err := openCityStoreAt(dir)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := updatedStore.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if got := updated.Metadata["session_key"]; got != want {
				t.Fatalf("session_key = %q, want %q; stderr: %s", got, want, stderr.String())
			}
		})
	}
}
