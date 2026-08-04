package worker

import (
	"reflect"
	"strings"
	"testing"
)

func TestProviderNativeForbiddenTokensReturnsACopy(t *testing.T) {
	first := ProviderNativeForbiddenTokens()
	if len(first) == 0 {
		t.Fatal("expected a non-empty canonical denylist")
	}
	first[0] = "mutated"
	if ProviderNativeForbiddenTokens()[0] == "mutated" {
		t.Fatal("ProviderNativeForbiddenTokens must return a defensive copy")
	}
}

func TestForbiddenTokensCannotCollideWithNeutralKeys(t *testing.T) {
	// Each native token must not be a substring of a legitimate snake_case
	// neutral key, otherwise a substring scan would false-positive. These are
	// the neutral keys most at risk of collision.
	neutral := []string{
		"tool_call_id", "exit_code", "file_path", "total_lines", "multi_select",
		"task_type", "task_id", "active_form", "new_todos", "old_todos",
		"user_modified", "replace_all", "applied_limit", "provider_session_id",
	}
	for _, token := range ProviderNativeForbiddenTokens() {
		for _, key := range neutral {
			if strings.Contains(key, token) {
				t.Fatalf("denylist token %q is a substring of neutral key %q; it would false-positive", token, key)
			}
		}
	}
}

func TestScanForbiddenTokensFindsLeaksAndExtras(t *testing.T) {
	clean := []byte(`{"file_path":"a.go","exit_code":0,"tool_call_id":"x"}`)
	if got := ScanForbiddenTokens(clean); got != nil {
		t.Fatalf("clean wire flagged tokens: %v", got)
	}

	leaked := []byte(`{"toolUseResult":{"exitCode":1},"file_path":"a.go"}`)
	got := ScanForbiddenTokens(leaked)
	if len(got) != 2 || got[0] != "exitCode" || got[1] != "toolUseResult" {
		t.Fatalf("expected [exitCode toolUseResult], got %v", got)
	}

	if got := ScanForbiddenTokens(clean, "shutdown_complete"); got != nil {
		t.Fatalf("extra token false-positive: %v", got)
	}
	withExtra := []byte(`{"text":"shutdown_complete happened"}`)
	if got := ScanForbiddenTokens(withExtra, "shutdown_complete"); len(got) != 1 || got[0] != "shutdown_complete" {
		t.Fatalf("expected [shutdown_complete], got %v", got)
	}
}

type wireSample struct {
	ID        string               `json:"id"`
	Text      string               `json:"text,omitempty"`
	Arguments []wireSampleArgument `json:"arguments,omitempty"`
	Hidden    string               `json:"-"`
	internal  string               //nolint:unused // exercises unexported-field skipping
	Nested    *wireSampleLeaf      `json:"nested,omitempty"`
	Items     []wireSampleLeaf     `json:"items,omitempty"`
}

type wireSampleLeaf struct {
	Kind string `json:"kind"`
}

type wireSampleArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func TestNeutralWireKeysWalksTheTypeTree(t *testing.T) {
	allowed := NeutralWireKeys(reflect.TypeOf(wireSample{}))
	for _, want := range []string{"id", "text", "arguments", "name", "value", "nested", "items", "kind"} {
		if _, ok := allowed[want]; !ok {
			t.Fatalf("expected key %q in allowlist, got %v", want, allowed)
		}
	}
	if _, ok := allowed["-"]; ok {
		t.Fatal("json:\"-\" field must not contribute a key")
	}
	if _, ok := allowed["internal"]; ok {
		t.Fatal("unexported field must not contribute a key")
	}
}

func TestUnexpectedWireKeysDescendsIntoJSONStringArgumentValues(t *testing.T) {
	allowed := NeutralWireKeys(reflect.TypeOf(wireSample{}))
	wire := []byte(`{"id":"a","arguments":[{"name":"action","value":"[{\"someBrandNewProviderKey\":{\"deepNativeKey\":true}}]"}]}`)

	unexpected, err := UnexpectedWireKeys(wire, allowed)
	if err != nil {
		t.Fatalf("scan string carrier: %v", err)
	}
	want := []string{"deepNativeKey", "someBrandNewProviderKey"}
	if !reflect.DeepEqual(unexpected, want) {
		t.Fatalf("UnexpectedWireKeys() = %v, want nested stringified keys %v", unexpected, want)
	}
}

func TestUnexpectedWireKeysLeavesTypedJSONTextOpaque(t *testing.T) {
	allowed := NeutralWireKeys(reflect.TypeOf(wireSample{}))
	wire := []byte(`{"id":"a","text":"{\"user_supplied\":{\"nested\":true}}"}`)

	unexpected, err := UnexpectedWireKeys(wire, allowed)
	if err != nil {
		t.Fatalf("scan typed text carrier: %v", err)
	}
	if unexpected != nil {
		t.Fatalf("typed text carrier flagged keys: %v", unexpected)
	}
}

func TestUnexpectedWireKeysDetectsNonSchemaKeys(t *testing.T) {
	allowed := NeutralWireKeys(reflect.TypeOf(wireSample{}))

	clean := []byte(`{"id":"a","nested":{"kind":"x"},"items":[{"kind":"y"}]}`)
	unexpected, err := UnexpectedWireKeys(clean, allowed)
	if err != nil {
		t.Fatalf("scan clean: %v", err)
	}
	if unexpected != nil {
		t.Fatalf("clean wire flagged keys: %v", unexpected)
	}

	dirty := []byte(`{"id":"a","nested":{"kind":"x","filePath":"a.go"},"items":[{"kind":"y","toolUseResult":1}]}`)
	unexpected, err = UnexpectedWireKeys(dirty, allowed)
	if err != nil {
		t.Fatalf("scan dirty: %v", err)
	}
	if len(unexpected) != 2 || unexpected[0] != "filePath" || unexpected[1] != "toolUseResult" {
		t.Fatalf("expected [filePath toolUseResult], got %v", unexpected)
	}
}
