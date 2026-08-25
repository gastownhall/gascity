package events

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestBackendCredentialResolvedPayloadOmitsTheCredential is the invariant the
// BackendCredentialResolved doc comment cites: the payload locates the endpoint
// a credential was resolved for and never carries the credential.
//
// gc no longer owns an emitter for this event — a bound scope's credential is
// resolved inside the linked beads library — so there is no resolver to run a
// canary through. The type is what crosses the wire, so the type is what this
// pins: the field set is closed, and widening it is a deliberate edit that must
// come back through here.
func TestBackendCredentialResolvedPayloadOmitsTheCredential(t *testing.T) {
	want := []string{"backend", "host", "port", "scope_kind", "scope_name", "source", "user"}

	typ := reflect.TypeOf(BackendCredentialResolvedPayload{})
	got := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "" {
			t.Fatalf("field %s has no json tag: an untagged field crosses the wire under its Go name", field.Name)
		}
		got = append(got, tag)
		if field.Type.Kind() != reflect.String {
			t.Errorf("field %s is %s: every field here is an identifier an operator reads", field.Name, field.Type)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload fields = %v, want %v: a new field must be shown to carry no credential before it is added", got, want)
	}

	// A populated payload marshals to exactly those keys, so a credential
	// cannot arrive through an embedded type or a custom marshaller either.
	canary := "backend-credential-canary"
	encoded, err := json.Marshal(BackendCredentialResolvedPayload{
		Backend:   canary,
		ScopeKind: "rig",
		ScopeName: canary,
		Source:    canary,
		Host:      "db.example.test",
		Port:      "5432",
		User:      "bd",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("payload JSON keys = %v, want %v", keys, want)
	}
	for _, forbidden := range []string{"password", "secret", "token", "credential", "dsn"} {
		if _, ok := decoded[forbidden]; ok {
			t.Errorf("payload JSON carries %q: %s", forbidden, encoded)
		}
	}
}

// TestBackendCredentialResolvedIsRegistered pins that the surviving wire type
// stays reachable from its event constant. The type outlived its emitter
// because it is a variant of the SSE payload union and a schema in the
// generated OpenAPI document; an unregistered constant would fork that
// contract between two assemblies of the same product.
func TestBackendCredentialResolvedIsRegistered(t *testing.T) {
	payload, ok := LookupPayload(BackendCredentialResolved)
	if !ok {
		t.Fatalf("event %q has no registered payload", BackendCredentialResolved)
	}
	if _, ok := payload.(BackendCredentialResolvedPayload); !ok {
		t.Fatalf("registered payload for %q is %T, want BackendCredentialResolvedPayload", BackendCredentialResolved, payload)
	}
}
