package connect_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/connect"
)

// TestStorageClassesMatchConfig keeps the bundle's class vocabulary equal to
// config.StorageClasses' toml keys, in order: a seventh class fails here. It
// lives in the external test package so internal/config may import connect
// (for ValidateLedgerURL) without an import cycle in connect's tests.
func TestStorageClassesMatchConfig(t *testing.T) {
	typ := reflect.TypeFor[config.StorageClasses]()
	var tags []string
	for i := range typ.NumField() {
		tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("toml"), ",")
		tags = append(tags, tag)
	}
	if got := connect.StorageClasses(); !slices.Equal(got, tags) {
		t.Fatalf("StorageClasses() = %q, config.StorageClasses keys = %q", got, tags)
	}
	got := connect.StorageClasses()
	got[0] = "mutated"
	if connect.StorageClasses()[0] != "work" {
		t.Fatal("StorageClasses returned shared storage")
	}
}

// TestWorkBindingMatchesConfig pins the city ledger's binding to config's
// reserved work binding, so a rename on either side fails here.
func TestWorkBindingMatchesConfig(t *testing.T) {
	if connect.WorkBinding != config.StorageWorkBinding {
		t.Fatalf("connect.WorkBinding = %q, config.StorageWorkBinding = %q", connect.WorkBinding, config.StorageWorkBinding)
	}
}
