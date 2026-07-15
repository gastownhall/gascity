package commandcensus

import (
	"bytes"
	"strings"
	"testing"
)

const testResultSchema = `{
  "properties": {
    "events": {"items": {"properties": {
      "command_id": {"enum": ["help", "version", "unknown", "pack-command"]}
    }}}
  }
}
`

func TestGenerateArtifactsIsDeterministicAndEmitsTombstoneDecodeEntry(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Tombstones = []Tombstone{{Name: "retired-command", ID: 6, Wire: "retired-command"}}
	manifest.NextID = 7

	first, err := GenerateArtifacts(manifest, []byte(testResultSchema))
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateArtifacts(manifest, []byte(testResultSchema))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("generation is not deterministic")
	}
	for name, artifact := range map[string]string{
		"runtime": first.RuntimeGo,
		"catalog": first.CatalogGo,
		"schema":  first.SchemaJSON,
	} {
		if strings.Contains(artifact, "retired-command") == (name == "runtime") {
			t.Fatalf("%s tombstone presence is wrong", name)
		}
	}
	if !strings.Contains(first.CatalogGo, `yield(commandIDEntry{id: generatedCommandID6, wire: "retired-command"})`) {
		t.Fatal("catalog does not retain the tombstone decode entry")
	}
	ledger, err := ParseGeneratedAllocationLedger([]byte(first.CatalogGo))
	if err != nil {
		t.Fatalf("parse generated allocation ledger: %v", err)
	}
	if len(ledger.Identities) != 2 || ledger.Identities[1].ID != 6 || !ledger.Identities[1].Retired {
		t.Fatalf("generated tombstone ledger entry = %+v, want id 6 retired", ledger.Identities)
	}
	reactivated := manifest.DeepCopy()
	reactivated.Tombstones = nil
	reactivatedCommand := reactivated.Commands[1]
	reactivatedCommand.Path = "gc retired-command"
	reactivatedCommand.Classification = "retired-command"
	reactivatedCommand.CanonicalIdentity = false
	reactivatedCommand.CanonicalTarget = ""
	reactivatedCommand.Mode = ModeStandard
	reactivatedCommand.NoticePolicy = NoticeEligible
	reactivatedCommand.ID = 6
	reactivated.Commands = append(reactivated.Commands, reactivatedCommand)
	if err := ValidateEvolution(ledger, reactivated); err == nil {
		t.Fatal("generated tombstone was accepted as an active identity")
	}
	if !strings.Contains(first.SchemaJSON, `"completion"`) || !strings.Contains(first.SchemaJSON, `"retired-command"`) {
		t.Fatal("schema enum does not include the complete decode catalog")
	}
	if !bytes.HasSuffix([]byte(first.SchemaJSON), []byte("\n")) {
		t.Fatal("schema does not end with one newline")
	}
}

func TestGeneratedCatalogYieldsOnlyGeneratedIDsOnce(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := GenerateArtifacts(manifest, []byte(testResultSchema))
	if err != nil {
		t.Fatal(err)
	}
	for _, permanent := range []string{"CommandHelp", "CommandVersion", "CommandUnknown", "CommandPackCommand"} {
		if strings.Contains(artifacts.CatalogGo, "yield(commandIDEntry{id: "+permanent) {
			t.Fatalf("generated seam duplicates permanent %s", permanent)
		}
	}
	if got := strings.Count(artifacts.CatalogGo, `yield(commandIDEntry{id: generatedCommandID5, wire: "completion"})`); got != 1 {
		t.Fatalf("completion yields = %d, want 1", got)
	}
}

func TestValidateEvolutionRequiresRemovedIdentityTombstone(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := GenerateArtifacts(manifest, []byte(testResultSchema))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := ParseGeneratedAllocationLedger([]byte(artifacts.CatalogGo))
	if err != nil {
		t.Fatal(err)
	}

	removed := manifest.DeepCopy()
	removed.Commands = removed.Commands[:1]
	if err := ValidateEvolution(previous, removed); err == nil {
		t.Fatal("removed identity was accepted without a tombstone")
	}
	removed.Tombstones = []Tombstone{{Name: "completion", ID: 5, Wire: "completion"}}
	if err := ValidateEvolution(previous, removed); err != nil {
		t.Fatalf("retained tombstone: %v", err)
	}
}

func TestValidateEvolutionRejectsRemapAndNextIDDecrease(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	for name, previous := range map[string]AllocationLedger{
		"id to wire remap":       {NextID: 6, Identities: []Identity{{Name: "old", ID: 5, Wire: "old"}}},
		"wire to id remap":       {NextID: 7, Identities: []Identity{{Name: "completion", ID: 6, Wire: "completion"}}},
		"name remap":             {NextID: 6, Identities: []Identity{{Name: "old-completion", ID: 5, Wire: "completion"}}},
		"tombstone reactivation": {NextID: 6, Identities: []Identity{{Name: "completion", ID: 5, Wire: "completion", Retired: true}}},
		"next id decrease":       {NextID: 7, Identities: []Identity{{Name: "completion", ID: 5, Wire: "completion"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEvolution(previous, manifest); err == nil {
				t.Fatal("ValidateEvolution accepted allocation history drift")
			}
		})
	}
}

func TestGenerateArtifactsUsesCollisionProofNumericIdentifiers(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Tombstones = []Tombstone{{Name: "i-d-entry", ID: 6, Wire: "retired-pack-command"}}
	manifest.NextID = 7
	artifacts, err := GenerateArtifacts(manifest, []byte(testResultSchema))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(artifacts.CatalogGo, "generatedCommandID6") || strings.Contains(artifacts.CatalogGo, "commandIDEntry CommandID") {
		t.Fatal("catalog did not use the numeric generated namespace")
	}
}

func TestGenerateArtifactsUsesCollisionProofRuntimeIdentifiers(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Tombstones = []Tombstone{{Name: "i-d", ID: 6, Wire: "retired-id"}}
	manifest.NextID = 7
	artifacts, err := GenerateArtifacts(manifest, []byte(testResultSchema))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(artifacts.RuntimeGo, "productMetricsGeneratedCommandID5") || strings.Contains(artifacts.RuntimeGo, "productMetricsGeneratedCommandID6") || strings.Contains(artifacts.RuntimeGo, "productMetricsCommandID productMetricsCommandID") {
		t.Fatal("runtime table did not use the numeric generated namespace")
	}
}

func TestParseGeneratedAllocationLedgerRejectsMalformedOrMissingMarkers(t *testing.T) {
	for name, data := range map[string][]byte{
		"malformed":             []byte("package productmetrics\n// command-census-allocation: broken\n"),
		"missing":               []byte("package productmetrics\n"),
		"null identities":       []byte("package productmetrics\n// command-census-ledger: {\"next_id\":5,\"identities\":null}\n"),
		"missing retired state": []byte("package productmetrics\n// command-census-ledger: {\"next_id\":6,\"identities\":[{\"name\":\"completion\",\"id\":5,\"wire\":\"completion\"}]}\n"),
		"null retired state":    []byte("package productmetrics\n// command-census-ledger: {\"next_id\":6,\"identities\":[{\"name\":\"completion\",\"id\":5,\"wire\":\"completion\",\"retired\":null}]}\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseGeneratedAllocationLedger(data); err == nil {
				t.Fatal("ParseGeneratedAllocationLedger accepted invalid history")
			}
		})
	}
	if ledger, err := ParseGeneratedAllocationLedger([]byte(S1BootstrapCatalog)); err != nil || !ledger.Bootstrap {
		t.Fatalf("S1 bootstrap ledger = %+v, err=%v", ledger, err)
	}
}
