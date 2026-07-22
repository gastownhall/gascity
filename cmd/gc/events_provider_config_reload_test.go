package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestEffectiveEventsProviderConfigHonorsOverrideAndSemanticCapabilities(t *testing.T) {
	t.Run("GC_EVENTS masks TOML provider edits", func(t *testing.T) {
		t.Setenv("GC_EVENTS", "fake")
		left := effectiveEventsProviderConfig(config.EventsConfig{Provider: "exec:/one"})
		right := effectiveEventsProviderConfig(config.EventsConfig{Provider: "exec:/two"})
		if left != right {
			t.Fatalf("effective configs differ under GC_EVENTS override: left=%+v right=%+v", left, right)
		}
	})

	t.Run("rotation is ignored for non-file providers", func(t *testing.T) {
		t.Setenv("GC_EVENTS", "")
		disabled := false
		left := effectiveEventsProviderConfig(config.EventsConfig{Provider: "fake"})
		right := effectiveEventsProviderConfig(config.EventsConfig{
			Provider: "fake",
			Rotation: config.EventsRotationConfig{Enabled: &disabled, ArchiveRetainAge: "24h"},
		})
		if left != right {
			t.Fatalf("fake-provider configs differ only by ignored rotation: left=%+v right=%+v", left, right)
		}
	})

	t.Run("file aliases canonicalize and effective rotation remains significant", func(t *testing.T) {
		t.Setenv("GC_EVENTS", "")
		base := effectiveEventsProviderConfig(config.EventsConfig{})
		alias := effectiveEventsProviderConfig(config.EventsConfig{Provider: "file"})
		unknown := effectiveEventsProviderConfig(config.EventsConfig{Provider: "legacy-file-name"})
		if base != alias || base != unknown {
			t.Fatalf("file aliases did not canonicalize: base=%+v alias=%+v unknown=%+v", base, alias, unknown)
		}

		disabled := false
		changed := effectiveEventsProviderConfig(config.EventsConfig{
			Rotation: config.EventsRotationConfig{Enabled: &disabled},
		})
		if changed == base {
			t.Fatalf("effective file rotation change was ignored: base=%+v changed=%+v", base, changed)
		}
	})
}
