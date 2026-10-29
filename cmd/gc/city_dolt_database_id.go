package main

import "path/filepath"

// cityDoltDatabaseIDPath is where a city's minted Dolt database id lives.
func cityDoltDatabaseIDPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "dolt-database-id")
}

// mintCityDoltDatabaseID is a signature stub for the RED commit: it mints
// nothing yet. The GREEN commit replaces it with the write-once implementation.
func mintCityDoltDatabaseID(string) error {
	return nil
}
