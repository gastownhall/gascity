package main

import (
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// lockCityConfig takes the city config lock (configedit.LockCity) for a gc
// command's config edit. When another process holds the lock it writes one
// notice naming the lock file to out, prefixed with cmdName, and waits.
//
// The command takes it immediately before its first read of the files it
// rewrites and releases it after its last write and any rollback. It never
// holds it while it calls the controller API or waits on the controller,
// which takes the same lock for its API edits.
func lockCityConfig(fs fsys.FS, cityPath, cmdName string, out io.Writer) (release func(), err error) {
	lockPath := configedit.CityLockFile(cityPath)
	return configedit.LockCity(fs, cityPath, func() {
		fmt.Fprintf(out, "%s: waiting for another config edit of this city to finish (%s)\n", cmdName, lockPath) //nolint:errcheck // best-effort diagnostic
	})
}

// lockCityConfigForCommand is lockCityConfig for a command that reports its
// own failures: when the lock cannot be taken it prints the error, prefixed
// with cmdName, to stderr and reports false, and the command must then return
// 1 without touching any config file.
func lockCityConfigForCommand(fs fsys.FS, cityPath, cmdName string, stderr io.Writer) (release func(), ok bool) {
	release, err := lockCityConfig(fs, cityPath, cmdName, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, false
	}
	return release, true
}

// lockImportRootConfigForCommand is lockCityConfigForCommand for the gc import
// commands, whose root is a city or a standalone pack. A pack has no city
// processes to serialize against, so a pack root takes no lock.
func lockImportRootConfigForCommand(fs fsys.FS, root, cmdName string, stderr io.Writer) (release func(), ok bool) {
	if !citylayout.HasCityConfig(root) {
		return func() {}, true
	}
	return lockCityConfigForCommand(fs, root, cmdName, stderr)
}

// lockCityConfigForDoctorFix takes the city config lock around one gc doctor
// --fix repair's read through its write. Each fix takes and releases the lock
// on its own; its wait notice goes to the doctor's output stream (stderr for a
// direct Fix call) and a lock failure is the fix's error.
func lockCityConfigForDoctorFix(ctx *doctor.CheckContext, cityPath string) (release func(), err error) {
	out := io.Writer(os.Stderr)
	if ctx != nil && ctx.Output != nil {
		out = ctx.Output
	}
	return lockCityConfig(fsys.OSFS{}, cityPath, "gc doctor --fix", out)
}
