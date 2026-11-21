package productmetrics

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	gascity "github.com/gastownhall/gascity"
)

const statusResultSchemaPath = "schemas/metrics/status/result.schema.json"

// The published status schema and the StateReason domain live in different
// files and nothing links them at compile time: "development-build" sat in the
// schema for the whole life of ReasonUnofficialBuild ("unofficial-build"), so
// every status call on an unofficial build emitted a payload the schema
// rejected. Compare the two sets directly so they cannot drift again in either
// direction.
func TestStatusReasonDomainMatchesPublishedSchema(t *testing.T) {
	declared := declaredStateReasons(t)
	published := publishedStatusReasonEnum(t)
	slices.Sort(declared)
	slices.Sort(published)
	if !slices.Equal(declared, published) {
		t.Fatalf("StateReason values and %s disagree.\ndeclared:  %v\npublished: %v", statusResultSchemaPath, declared, published)
	}
}

func declaredStateReasons(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "service.go", nil, 0)
	if err != nil {
		t.Fatalf("parse service.go: %v", err)
	}
	var reasons []string
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || !stateReasonTyped(value.Type) {
				continue
			}
			if len(value.Names) != 1 || len(value.Values) != 1 {
				t.Fatalf("StateReason declaration %v must bind exactly one string literal", value.Names)
			}
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("StateReason %s must be a string literal so the schema gate can read it", value.Names[0].Name)
			}
			unquoted, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", literal.Value, err)
			}
			reasons = append(reasons, unquoted)
		}
	}
	if len(reasons) == 0 {
		t.Fatal("found no StateReason constants in service.go")
	}
	return reasons
}

func stateReasonTyped(expr ast.Expr) bool {
	identifier, ok := expr.(*ast.Ident)
	return ok && identifier.Name == "StateReason"
}

func publishedStatusReasonEnum(t *testing.T) []string {
	t.Helper()
	data, err := gascity.BuiltinSchemas.ReadFile(statusResultSchemaPath)
	if err != nil {
		t.Fatalf("read embedded %s: %v", statusResultSchemaPath, err)
	}
	var schema struct {
		Properties struct {
			Reason struct {
				Enum []string `json:"enum"`
			} `json:"reason"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("decode %s: %v", statusResultSchemaPath, err)
	}
	if len(schema.Properties.Reason.Enum) == 0 {
		t.Fatalf("%s declares no reason enum", statusResultSchemaPath)
	}
	return schema.Properties.Reason.Enum
}
