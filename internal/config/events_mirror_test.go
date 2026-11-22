package config

import (
	"os"
	"strings"
	"testing"
)

func TestEventsMirrorConfigRoundTrip(t *testing.T) {
	maxSize := int64(5000)
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Events: EventsConfig{Mirror: EventsMirrorConfig{
			Path:         "/var/log/gascity/hq/events.jsonl",
			Mode:         "0600",
			MaxSizeBytes: &maxSize,
		}},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		"[events.mirror]",
		`path = "/var/log/gascity/hq/events.jsonl"`,
		`mode = "0600"`,
		"max_size_bytes = 5000",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("marshaled config missing %q:\n%s", want, data)
		}
	}
	decoded, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := decoded.Events.Mirror
	if m.Path != "/var/log/gascity/hq/events.jsonl" || m.Mode != "0600" || m.MaxSizeBytes == nil || *m.MaxSizeBytes != 5000 {
		t.Fatalf("decoded mirror = %+v", m)
	}
}

func TestEventsMirrorConfigUnsetMarshalsNothing(t *testing.T) {
	cfg := &City{Workspace: Workspace{Name: "test-city"}}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "mirror") {
		t.Fatalf("unset mirror appears in marshaled config:\n%s", data)
	}
	if cfg.Events.Mirror.Enabled() {
		t.Fatal("unset mirror reports enabled")
	}
	if got := ValidateEventsRotation(cfg); len(got) != 0 {
		t.Fatalf("ValidateEventsRotation = %v, want none", got)
	}
}

func TestEventsMirrorConfigDefaultsAndMode(t *testing.T) {
	var m EventsMirrorConfig
	if got := m.MaxSizeBytesOrDefault(); got != DefaultEventsMirrorMaxSizeBytes {
		t.Fatalf("MaxSizeBytesOrDefault = %d, want %d", got, DefaultEventsMirrorMaxSizeBytes)
	}
	for raw, want := range map[string]os.FileMode{"": 0o640, "0640": 0o640, "600": 0o600, "0644": 0o644} {
		got, err := EventsMirrorConfig{Mode: raw}.ModeOrDefault()
		if err != nil || got != want {
			t.Fatalf("ModeOrDefault(%q) = %#o, %v; want %#o", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "0888", "02750", "rw-r-----", "-1"} {
		if _, err := (EventsMirrorConfig{Mode: raw}).ModeOrDefault(); err == nil {
			t.Fatalf("ModeOrDefault(%q) accepted an invalid mode", raw)
		}
	}
}

func TestValidateEventsRotationWarnsOnUnusableMirror(t *testing.T) {
	cfg := &City{Events: EventsConfig{Mirror: EventsMirrorConfig{Path: "relative/events.jsonl"}}}
	got := ValidateEventsRotation(cfg)
	if len(got) != 1 || !strings.HasPrefix(got[0], "events.mirror: warning: ") || !strings.Contains(got[0], "not absolute") {
		t.Fatalf("ValidateEventsRotation = %v, want one events.mirror warning", got)
	}
	cfg.Events.Mirror.Path = "/var/log/gascity/hq/events.jsonl"
	if got := ValidateEventsRotation(cfg); len(got) != 0 {
		t.Fatalf("ValidateEventsRotation = %v for a valid mirror, want none", got)
	}
}
