package main

import "github.com/gastownhall/gascity/internal/session"

// This package's tests predate the runtime lease and run session Managers
// without a city path.
func init() { session.AllowManagersWithoutCityForTest() }
