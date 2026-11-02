package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestCityStatusJSONSchemaDeclaresEveryField keeps the published
// `gc status --json` schema in step with the Go types the command marshals,
// including the ones other packages own (the bead store diagnostic, the
// conditional-writes block). At every object level the schema declares
// exactly the struct's JSON fields, requires exactly the ones the struct
// always emits, types each field as the struct does, copies every enum tag,
// and stays closed. A field added to any of those types without a schema
// entry is the drift #8453 found four times over; it fails here first.
func TestCityStatusJSONSchemaDeclaresEveryField(t *testing.T) {
	raw, err := readBuiltinSchema([]string{"status"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read status schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse status schema: %v", err)
	}
	assertCityStatusSchemaMatchesType(t, "$", schema, reflect.TypeOf(StatusJSON{}))
}

// assertCityStatusSchemaMatchesType walks one schema node against the Go type
// it describes, recursing through pointers, slice items and struct fields.
func assertCityStatusSchemaMatchesType(t *testing.T, path string, node map[string]any, typ reflect.Type) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	wantType := map[reflect.Kind]string{
		reflect.Struct: "object", reflect.Slice: "array", reflect.String: "string", reflect.Bool: "boolean",
		reflect.Int: "integer", reflect.Int64: "integer", reflect.Float64: "number",
	}[typ.Kind()]
	if wantType == "" {
		t.Errorf("%s: %s has kind %s, which this check does not map to a JSON type", path, typ, typ.Kind())
		return
	}
	if _, isConst := node["const"]; !isConst && node["type"] != wantType {
		t.Errorf("%s: schema type = %v, want %s for %s", path, node["type"], wantType, typ)
	}
	switch typ.Kind() {
	case reflect.Slice:
		items, ok := node["items"].(map[string]any)
		if !ok {
			t.Errorf("%s: array declares no items", path)
			return
		}
		assertCityStatusSchemaMatchesType(t, path+"[]", items, typ.Elem())
	case reflect.Struct:
		if node["additionalProperties"] != false {
			t.Errorf("%s: object is not closed (additionalProperties = %v)", path, node["additionalProperties"])
		}
		props, _ := node["properties"].(map[string]any)
		undeclaredInGo := make(map[string]bool, len(props))
		for name := range props {
			undeclaredInGo[name] = true
		}
		required := map[string]bool{}
		reqList, _ := node["required"].([]any)
		for _, r := range reqList {
			required[r.(string)] = true
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			name, opts, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				t.Errorf("%s: %s.%s has no JSON name", path, typ, field.Name)
				continue
			}
			delete(undeclaredInGo, name)
			child, ok := props[name].(map[string]any)
			if !ok {
				t.Errorf("%s.%s: %s.%s emits it but the schema does not declare it", path, name, typ, field.Name)
				continue
			}
			omitempty := strings.Contains(","+opts+",", ",omitempty,")
			if required[name] == omitempty {
				t.Errorf("%s.%s: schema required = %v, but %s.%s omitempty = %v", path, name, required[name], typ, field.Name, omitempty)
			}
			if enum := field.Tag.Get("enum"); enum != "" {
				want := strings.Split(enum, ",")
				var got []string
				schemaEnum, _ := child["enum"].([]any)
				for _, v := range schemaEnum {
					got = append(got, v.(string))
				}
				sort.Strings(want)
				sort.Strings(got)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s.%s: schema enum = %v, %s.%s enum tag = %v", path, name, got, typ, field.Name, want)
				}
			}
			assertCityStatusSchemaMatchesType(t, path+"."+name, child, field.Type)
		}
		for name := range undeclaredInGo {
			t.Errorf("%s.%s: the schema declares it but %s has no such field", path, name, typ)
		}
	}
}
