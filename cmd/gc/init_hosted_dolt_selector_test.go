package main

import (
	"strings"
	"testing"
)

// A selector-driven external init never consumes --dolt-project-id: the
// identity write is reached only on the legacy --dolt-host path
// (cmd_init.go gates it on !selectorRequested), the ownership journal records
// host/port/database only, and the provider script passes bd just --database.
// bd resolves project_id itself, adopting the hosted database's _project_id or
// minting one. Requiring the flag made the new front door refuse its own
// documented invocation, and accepting it would have pinned an identity gc
// then discarded.
func TestSelectorExternalInitDoesNotRequireProjectID(t *testing.T) {
	for _, transport := range []string{"direct", "proxied"} {
		t.Run(transport, func(t *testing.T) {
			o := hostedDoltInitOptions{
				Transport: transport,
				Target:    "external",
				Host:      "db.example",
				Port:      "3306",
				Database:  "project",
			}
			if err := o.validate(); err != nil {
				t.Fatalf("selector external init refused host+port+database: %v", err)
			}
		})
	}
}

// The database is a different question: it names WHICH database on a server
// somebody else operates. bd would otherwise derive one from the issue prefix
// and attach to, or create, the wrong database on a shared server — so it stays
// required, and the refusal says which flag to pass.
func TestSelectorExternalInitStillRequiresDatabase(t *testing.T) {
	o := hostedDoltInitOptions{Transport: "proxied", Target: "external", Host: "db.example", Port: "3306"}
	err := o.validate()
	if err == nil {
		t.Fatal("selector external init accepted an unnamed external database")
	}
	if !strings.Contains(err.Error(), "--dolt-database") {
		t.Fatalf("refusal = %v, want it to name --dolt-database", err)
	}
}

// The legacy --dolt-host alias keeps the full contract: its project id IS
// consumed, by contract.WriteProjectIdentity.
func TestLegacyDoltHostInitStillRequiresProjectID(t *testing.T) {
	o := hostedDoltInitOptions{Host: "db.example", Port: "3306", Database: "project"}
	err := o.validate()
	if err == nil {
		t.Fatal("legacy --dolt-host init accepted a missing project id")
	}
	if !strings.Contains(err.Error(), "--dolt-project-id") {
		t.Fatalf("refusal = %v, want it to name --dolt-project-id", err)
	}
}
