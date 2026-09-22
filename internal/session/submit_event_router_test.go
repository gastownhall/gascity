package session

import (
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// Compile-time checks that the composite providers this fix targets still
// implement eventCapableRouter. A refactor that drops the method must fail
// the build here instead of silently falling back to the top-level
// SessionEventProvider assertion these checks exist to bypass. Kept in a
// test file: production code has no reason to import auto/hybrid directly,
// only to route through the generic runtime.Provider interface.
var (
	_ eventCapableRouter = (*sessionauto.Provider)(nil)
	_ eventCapableRouter = (*sessionhybrid.Provider)(nil)
)
