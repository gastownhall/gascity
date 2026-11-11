package main

import (
	"errors"

	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeOwnerSource is not implemented yet.
func (c *ownerlessRuntimesCheck) runtimeOwnerSource() doctor.RuntimeOwnerSource {
	return func() (doctor.RuntimeOwnerResolver, error) { return nil, errors.New("not implemented") }
}

// runtimeOwnerResolver is not implemented yet.
func runtimeOwnerResolver(_ *sessionCensus, _ runtime.Provider) doctor.RuntimeOwnerResolver {
	return func(string) (doctor.RuntimeOwnership, string) { return doctor.RuntimeUnproven, "not implemented" }
}
