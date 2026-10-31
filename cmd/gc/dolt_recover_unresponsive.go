package main

import "path/filepath"

// managedDoltUnresponsiveSincePath is the file that records when a live
// managed Dolt was first seen unresponsive, so the window spans processes.
func managedDoltUnresponsiveSincePath(layout managedDoltRuntimeLayout) string {
	return filepath.Join(layout.PackStateDir, "recover-unresponsive-since")
}

// managedDoltRecoverCaller describes who asked for the recover.
func managedDoltRecoverCaller() string {
	return ""
}
