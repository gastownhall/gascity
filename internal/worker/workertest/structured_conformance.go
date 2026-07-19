package workertest

import (
	"encoding/json"
	"fmt"
	"reflect"

	worker "github.com/gastownhall/gascity/internal/worker"
)

// structuredBlocks flattens the blocks of every entry in a normalized history.
func structuredBlocks(history *worker.HistorySnapshot) []worker.HistoryBlock {
	if history == nil {
		return nil
	}
	var blocks []worker.HistoryBlock
	for _, entry := range history.Entries {
		blocks = append(blocks, entry.Blocks...)
	}
	return blocks
}

// StructuredToolResultResult (WC-STRUCT-001) asserts that the normalized history
// exposes at least one typed StructuredToolResult — i.e. a provider tool result
// reached the neutral structured carrier rather than being flattened to text.
func StructuredToolResultResult(profile ProfileID, history *worker.HistorySnapshot) Result {
	for _, block := range structuredBlocks(history) {
		if block.StructuredResult != nil && block.StructuredResult.Kind != "" {
			return Pass(profile, RequirementStructuredToolResult,
				"tool result normalized into a typed StructuredToolResult")
		}
	}
	return Fail(profile, RequirementStructuredToolResult,
		"no block carried a typed StructuredToolResult")
}

// StructuredNoNativeLeakResult (WC-STRUCT-002) asserts the typed structured
// carriers (StructuredToolInput / StructuredToolResult) expose no provider-native
// keys. Provider-native shape is preserved separately on the raw frame; it must
// not appear on the neutral structured data.
func StructuredNoNativeLeakResult(profile ProfileID, history *worker.HistorySnapshot) Result {
	inputAllowed := worker.NeutralWireKeys(reflect.TypeOf(worker.StructuredToolInput{}))
	resultAllowed := worker.NeutralWireKeys(reflect.TypeOf(worker.StructuredToolResult{}))
	for _, block := range structuredBlocks(history) {
		if r := scanStructuredCarrier(profile, block.StructuredInput, inputAllowed); !r.Passed() {
			return r
		}
		if r := scanStructuredCarrier(profile, block.StructuredResult, resultAllowed); !r.Passed() {
			return r
		}
	}
	return Pass(profile, RequirementStructuredNoNativeLeak,
		"typed structured carriers exposed no provider-native keys")
}

func scanStructuredCarrier(profile ProfileID, carrier any, allowed map[string]struct{}) Result {
	if value := reflect.ValueOf(carrier); !value.IsValid() || (value.Kind() == reflect.Ptr && value.IsNil()) {
		return Pass(profile, RequirementStructuredNoNativeLeak, "no carrier")
	}
	wire, err := json.Marshal(carrier)
	if err != nil {
		return Fail(profile, RequirementStructuredNoNativeLeak, "marshal structured carrier: "+err.Error())
	}
	if leaked := worker.ScanForbiddenTokens(wire); len(leaked) > 0 {
		return Fail(profile, RequirementStructuredNoNativeLeak,
			fmt.Sprintf("structured carrier leaked provider-native token(s) %v", leaked))
	}
	unexpected, err := worker.UnexpectedWireKeys(wire, allowed)
	if err != nil {
		return Fail(profile, RequirementStructuredNoNativeLeak, "scan carrier keys: "+err.Error())
	}
	if len(unexpected) > 0 {
		return Fail(profile, RequirementStructuredNoNativeLeak,
			fmt.Sprintf("structured carrier carried non-schema key(s) %v", unexpected))
	}
	return Pass(profile, RequirementStructuredNoNativeLeak, "carrier clean")
}

// StructuredEditEvidenceResult (WC-STRUCT-003) asserts that an edit result
// preserves the provider's result-side patch evidence as typed data. Combined
// with the worker-level no-fabrication guard, this proves edit diffs come from
// the provider rather than being synthesized from tool input. Profiles whose
// fixture has no edit result are reported out of scope.
func StructuredEditEvidenceResult(profile ProfileID, history *worker.HistorySnapshot) Result {
	for _, block := range structuredBlocks(history) {
		result := block.StructuredResult
		if result == nil || result.Kind != "edit" {
			continue
		}
		if len(result.PatchHunks) == 0 && result.Patch == "" {
			return Fail(profile, RequirementStructuredEditEvidence,
				"edit result carried no result-side patch evidence")
		}
		return Pass(profile, RequirementStructuredEditEvidence,
			"edit result preserved result-side patch evidence as typed data")
	}
	return Unsupported(profile, RequirementStructuredEditEvidence,
		"profile fixture has no edit result to evaluate")
}
