package commandcensus

import (
	"strings"
	"testing"
)

const validManifestJSON = `{
  "schema_version": 1,
  "next_id": 6,
  "permanent_ids": [
    {"name":"help","id":1,"wire":"help"},
    {"name":"version","id":2,"wire":"version"},
    {"name":"unknown","id":3,"wire":"unknown"},
    {"name":"pack-command","id":4,"wire":"pack-command"}
  ],
  "global_conditional_modes": ["generic-machine-output", "managed-context", "provider-hook"],
  "commands": [
    {"path":"gc","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable-group","classification":"help","canonical_target":"@help","mode":"standard","notice_policy":"eligible","recording_policy":"recordable","owner":"deferred","resolver":"root-dispatch","deferred_default":"help","id":1},
    {"path":"gc completion bash","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable","classification":"completion","canonical_identity":true,"mode":"completion","notice_policy":"ineligible","recording_policy":"recordable","owner":"immediate","id":5},
    {"path":"gc completion fish","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable","classification":"completion","canonical_target":"gc completion bash","mode":"completion","notice_policy":"ineligible","recording_policy":"recordable","owner":"immediate","id":5},
    {"path":"gc completion powershell","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable","classification":"completion","canonical_target":"gc completion bash","mode":"completion","notice_policy":"ineligible","recording_policy":"recordable","owner":"immediate","id":5},
    {"path":"gc completion zsh","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable","classification":"completion","canonical_target":"gc completion bash","mode":"completion","notice_policy":"ineligible","recording_policy":"recordable","owner":"immediate","id":5}
  ],
  "synthetic": [
    {"path":"gc <unknown>","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable","classification":"unknown","mode":"standard","notice_policy":"eligible","recording_policy":"recordable","owner":"deferred","resolver":"root-dispatch","id":3},
    {"path":"gc <pack-command>","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable","classification":"pack-command","mode":"pack-command","notice_policy":"ineligible","recording_policy":"recordable","owner":"deferred","resolver":"pack-dispatch","id":4},
    {"path":"gc __complete","aliases":["__completeNoDesc"],"conditional_modes":[],"hidden":true,"effective_hidden":true,"disable_flag_parsing":true,"shape":"runnable","classification":"excluded","mode":"private-completion","notice_policy":"ineligible","recording_policy":"excluded","owner":"excluded","exclusion":"private-completion"}
  ],
  "tombstones": []
}`

func TestDecodeManifestRejectsUnknownAndDuplicateJSONFields(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown": strings.Replace(validManifestJSON, `"schema_version": 1`, `"schema_version": 1, "surprise": true`, 1),
		"nested unknown": strings.Replace(validManifestJSON,
			`{"name":"help","id":1,"wire":"help"}`,
			`{"name":"help","id":1,"wire":"help","surprise":true}`, 1),
		"duplicate": strings.Replace(validManifestJSON, `"next_id": 6`, `"next_id": 6, "next_id": 6`, 1),
		"nested duplicate": strings.Replace(validManifestJSON,
			`{"name":"help","id":1,"wire":"help"}`,
			`{"name":"help","id":1,"id":1,"wire":"help"}`, 1),
		"null aliases":                 strings.Replace(validManifestJSON, `"aliases":[]`, `"aliases":null`, 1),
		"missing disable flag parsing": strings.Replace(validManifestJSON, `,"disable_flag_parsing":false`, ``, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeManifest([]byte(raw)); err == nil {
				t.Fatal("DecodeManifest accepted invalid JSON")
			}
		})
	}
}

func TestValidateManifestPinsPermanentIDsAndSyntheticRows(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*Manifest){
		"permanent drift":                func(m *Manifest) { m.PermanentIDs[0].ID = 9 },
		"missing unknown":                func(m *Manifest) { m.Synthetic = m.Synthetic[1:] },
		"missing private completion":     func(m *Manifest) { m.Synthetic = m.Synthetic[:2] },
		"private completion alias drift": func(m *Manifest) { m.Synthetic[2].Aliases = nil },
		"second pack wildcard":           func(m *Manifest) { m.Synthetic = append(m.Synthetic, m.Synthetic[1]) },
	} {
		t.Run(name, func(t *testing.T) {
			cloned := manifest.DeepCopy()
			mutate(&cloned)
			if err := ValidateManifest(cloned); err == nil {
				t.Fatal("ValidateManifest accepted invalid manifest")
			}
		})
	}
}

func TestValidateManifestEnforcesStableIDLedger(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*Manifest){
		"same id different wire": func(m *Manifest) {
			shared := m.Commands[1]
			shared.Path = "gc completion nushell"
			shared.Classification = "other"
			shared.CanonicalTarget = "gc completion bash"
			m.Commands = append(m.Commands, shared)
		},
		"same wire different id": func(m *Manifest) {
			shared := m.Commands[1]
			shared.Path = "gc completion nushell"
			shared.ID = 6
			shared.CanonicalTarget = "gc completion bash"
			m.Commands = append(m.Commands, shared)
			m.NextID = 7
		},
		"next id reuses active": func(m *Manifest) { m.NextID = 5 },
		"tombstone collides active": func(m *Manifest) {
			m.Tombstones = []Tombstone{{Name: "retired", ID: 5, Wire: "retired"}}
		},
		"tombstone below next id": func(m *Manifest) {
			m.Tombstones = []Tombstone{{Name: "retired", ID: 8, Wire: "retired"}}
			m.NextID = 8
		},
		"exclusion without reason": func(m *Manifest) {
			m.Commands[1].Mode = "private-completion"
			m.Commands[1].NoticePolicy = "ineligible"
			m.Commands[1].RecordingPolicy = "excluded"
			m.Commands[1].Classification = "excluded"
			m.Commands[1].Owner = "excluded"
			m.Commands[1].ID = 0
			m.Commands[1].Exclusion = ""
		},
		"recordable with reason":      func(m *Manifest) { m.Commands[1].Exclusion = "private-completion" },
		"deferred default mismatch":   func(m *Manifest) { m.Commands[0].DeferredDefault = DeferredDefaultUnknown },
		"excluded canonical identity": func(m *Manifest) { m.Synthetic[2].CanonicalIdentity = true },
		"missing global selector":     func(m *Manifest) { m.GlobalConditionalModes = m.GlobalConditionalModes[:2] },
	} {
		t.Run(name, func(t *testing.T) {
			cloned := manifest.DeepCopy()
			mutate(&cloned)
			if err := ValidateManifest(cloned); err == nil {
				t.Fatal("ValidateManifest accepted invalid ledger")
			}
		})
	}
}

func TestValidateManifestAllowsImmediateRunnableGroup(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Commands[1].Shape = ShapeRunnableGroup
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("immediate runnable group: %v", err)
	}
}

func TestValidateManifestAllowsSharedCanonicalIdentity(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("shared completion identity: %v", err)
	}
}

func TestValidateManifestRejectsUnreviewedHiddenAndCanonicalOverrides(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"hidden without exception": func(m *Manifest) { m.Commands[1].EffectiveHidden = true },
		"hidden exception on visible row": func(m *Manifest) {
			m.Commands[1].HiddenException = HiddenExceptionPerfWrapper
		},
		"arbitrary wire override": func(m *Manifest) {
			m.Commands[0].Classification = "invented"
			m.Commands[0].ID = 5
		},
		"live unknown sentinel": func(m *Manifest) {
			m.Commands[1].Classification = "unknown"
			m.Commands[1].ID = 3
		},
		"live pack sentinel": func(m *Manifest) {
			m.Commands[1].Classification = "pack-command"
			m.Commands[1].ID = 4
		},
	} {
		t.Run(name, func(t *testing.T) {
			cloned := manifest.DeepCopy()
			mutate(&cloned)
			if err := ValidateManifest(cloned); err == nil {
				t.Fatal("ValidateManifest accepted unreviewed override")
			}
		})
	}
}

func TestValidateManifestRejectsNonCanonicalOrderingAndAliases(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"unsorted rows":     func(m *Manifest) { m.Commands[1], m.Commands[2] = m.Commands[2], m.Commands[1] },
		"unsorted aliases":  func(m *Manifest) { m.Commands[0].Aliases = []string{"z", "a"} },
		"duplicate aliases": func(m *Manifest) { m.Commands[0].Aliases = []string{"alias", "alias"} },
		"canonical alias":   func(m *Manifest) { m.Commands[0].Aliases = []string{"gc"} },
		"invalid path":      func(m *Manifest) { m.Commands[1].Path = "gc  completion bash" },
	} {
		t.Run(name, func(t *testing.T) {
			cloned := manifest.DeepCopy()
			mutate(&cloned)
			if err := ValidateManifest(cloned); err == nil {
				t.Fatal("ValidateManifest accepted non-canonical ordering")
			}
		})
	}
}
