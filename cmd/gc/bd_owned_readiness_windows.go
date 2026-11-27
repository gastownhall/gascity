//go:build windows

package main

// cityDoltReadinessBelongsToBd reports whether bd, not gc, answers for the
// readiness of the city scope's Dolt server: the scope is provider-owned, so
// bd started its server and starts it again for any bd command that finds it
// down.
//
// While it holds, the bd environment builder does not fall back to a health
// check of the provider-owned scopes when it finds no Dolt target of gc's own:
// that check is one `bd ping` per scope for every bd command gc makes, and it
// can only find what the command itself is about to find.
func cityDoltReadinessBelongsToBd(cityPath string) bool {
	owned, err := cityScopeProviderOwned(cityPath)
	return err == nil && owned
}
