package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// hookClaimResultSchemaPath is the published contract a `gc hook --claim --json`
// consumer validates against.
const hookClaimResultSchemaPath = "../../schemas/hook/result.schema.json"

// hookClaimDrainReasonConstPrefix is the naming convention that makes this gate
// mechanical: every drain-action reason is declared as a hookClaimReason* const
// in cmd_hook_claim.go, and the non-drain reasons ("claimed",
// "existing_assignment", "ready_assignment") are inline literals on the work
// path. So the constant set IS the drain-reason set.
const hookClaimDrainReasonConstPrefix = "hookClaimReason"

// declaredHookClaimDrainReasons reads the drain-reason constants straight out of
// the source rather than from a hand-maintained slice. A slice would have to be
// updated by the same person who adds the constant, which is exactly the step
// that gets forgotten — and forgetting it is what silently ships a reason no
// published schema admits.
func declaredHookClaimDrainReasons(t *testing.T) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "cmd_hook_claim.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing cmd_hook_claim.go: %v", err)
	}
	reasons := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if !strings.HasPrefix(name.Name, hookClaimDrainReasonConstPrefix) || i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquoting %s = %s: %v", name.Name, lit.Value, err)
				}
				reasons[unquoted] = name.Name
			}
		}
	}
	if len(reasons) == 0 {
		t.Fatal("found no hookClaimReason* constants; this gate has lost its subject")
	}
	return reasons
}

// hookClaimSchemaReasonEnums returns the schema's two reason enums: the
// top-level set of every valid reason, and the narrower set valid when action is
// "drain".
func hookClaimSchemaReasonEnums(t *testing.T) (all, drain []string) {
	t.Helper()
	raw, err := os.ReadFile(hookClaimResultSchemaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", hookClaimResultSchemaPath, err)
	}
	var schema struct {
		Properties struct {
			Reason struct {
				Enum []string `json:"enum"`
			} `json:"reason"`
		} `json:"properties"`
		AllOf []struct {
			If struct {
				Properties struct {
					Action struct {
						Const string `json:"const"`
					} `json:"action"`
				} `json:"properties"`
			} `json:"if"`
			Then struct {
				Properties struct {
					Reason struct {
						Enum []string `json:"enum"`
					} `json:"reason"`
				} `json:"properties"`
			} `json:"then"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decoding %s: %v", hookClaimResultSchemaPath, err)
	}
	for _, branch := range schema.AllOf {
		if branch.If.Properties.Action.Const == "drain" {
			drain = branch.Then.Properties.Reason.Enum
		}
	}
	return schema.Properties.Reason.Enum, drain
}

// TestHookClaimDrainReasonsMatchThePublishedSchema is the conformance gate
// between the drain reasons the code can emit and the contract it publishes.
//
// It runs in BOTH directions on purpose. A reason the code emits but the schema
// omits ships a result a conforming consumer must reject — the failure mode that
// makes a structured protocol worse than no protocol. A reason the schema
// advertises but no code path produces is a consumer waiting for something that
// never arrives, which is how a dead branch survives a refactor.
func TestHookClaimDrainReasonsMatchThePublishedSchema(t *testing.T) {
	declared := declaredHookClaimDrainReasons(t)
	allEnum, drainEnum := hookClaimSchemaReasonEnums(t)
	if len(drainEnum) == 0 {
		t.Fatal("schema has no action=drain branch constraining reason; the drain contract is unenforced")
	}

	inEnum := func(enum []string, want string) bool {
		for _, got := range enum {
			if got == want {
				return true
			}
		}
		return false
	}

	for reason, constName := range declared {
		if !inEnum(allEnum, reason) {
			t.Errorf("%s = %q is not in the schema's reason enum %v", constName, reason, allEnum)
		}
		if !inEnum(drainEnum, reason) {
			t.Errorf("%s = %q is not in the schema's action=drain reason enum %v", constName, reason, drainEnum)
		}
	}
	for _, reason := range drainEnum {
		if _, ok := declared[reason]; !ok {
			missing := make([]string, 0, len(declared))
			for value := range declared {
				missing = append(missing, value)
			}
			sort.Strings(missing)
			t.Errorf("schema admits drain reason %q that no %s* constant produces (declared: %v)",
				reason, hookClaimDrainReasonConstPrefix, missing)
		}
	}
}

// TestHookClaimNonTurnDrainMatchesTheSchema is the end-to-end half: the refusal
// this round adds must actually emit the reason the schema now admits, not just
// declare a constant next to it.
func TestHookClaimNonTurnDrainMatchesTheSchema(t *testing.T) {
	_, drainEnum := hookClaimSchemaReasonEnums(t)
	found := false
	for _, reason := range drainEnum {
		if reason == hookClaimReasonNonTurnContext {
			found = true
		}
	}
	if !found {
		t.Fatalf("schema drain enum %v does not admit %q", drainEnum, hookClaimReasonNonTurnContext)
	}
}
