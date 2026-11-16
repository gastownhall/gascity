//go:build windows

package runtime

import "strings"

// envRedactionInertWindows extends [envRedactionInert] with the variables
// Windows sets in every session, for the same reason the POSIX names are there:
// each holds a path, a path list, or a machine, user or domain name; none of
// them authenticates anything; and their values are what a failing command's
// output is made of. Scrubbed, USERPROFILE alone turns every path under the
// user's home into `<redacted>\...`, and TEMP, APPDATA, LOCALAPPDATA and
// SystemRoot account for most of the rest.
//
// The OneDrive names come from the in-box OneDrive client rather than from
// Windows itself. They are here because Known Folder Move puts Desktop and
// Documents under them, so where it is on they prefix as many paths as
// USERPROFILE does.
//
// Like the POSIX names, none of these is argv-safe.
var envRedactionInertWindows = map[string]bool{
	// Per-user paths.
	"APPDATA":            true,
	"HOMEDRIVE":          true,
	"HOMEPATH":           true,
	"LOCALAPPDATA":       true,
	"OneDrive":           true,
	"OneDriveCommercial": true,
	"OneDriveConsumer":   true,
	"TEMP":               true,
	"TMP":                true,
	"USERPROFILE":        true,

	// System paths and path lists. Path is PATH, already in the POSIX set.
	"ALLUSERSPROFILE":         true,
	"CommonProgramFiles":      true,
	"CommonProgramFiles(x86)": true,
	"CommonProgramW6432":      true,
	"ComSpec":                 true,
	"DriverData":              true,
	"PATHEXT":                 true,
	"ProgramData":             true,
	"ProgramFiles":            true,
	"ProgramFiles(x86)":       true,
	"ProgramW6432":            true,
	"PSModulePath":            true,
	"PUBLIC":                  true,
	"SystemDrive":             true,
	"SystemRoot":              true,
	"windir":                  true,

	// Machine, user and domain names.
	"COMPUTERNAME":              true,
	"LOGONSERVER":               true,
	"SESSIONNAME":               true,
	"USERDNSDOMAIN":             true,
	"USERDOMAIN":                true,
	"USERDOMAIN_ROAMINGPROFILE": true,
	"USERNAME":                  true,

	// The machine itself.
	"NUMBER_OF_PROCESSORS":   true,
	"OS":                     true,
	"PROCESSOR_ARCHITECTURE": true,
	"PROCESSOR_IDENTIFIER":   true,
	"PROCESSOR_LEVEL":        true,
	"PROCESSOR_REVISION":     true,
}

// envRedactionInertFolded is every redaction-inert name, POSIX and Windows,
// upper-cased.
var envRedactionInertFolded = func() map[string]bool {
	folded := make(map[string]bool, len(envRedactionInert)+len(envRedactionInertWindows))
	for _, set := range []map[string]bool{envRedactionInert, envRedactionInertWindows} {
		for name := range set {
			folded[strings.ToUpper(name)] = true
		}
	}
	return folded
}()

// redactionInertEnvKey reports whether key names a redaction-inert variable.
//
// Windows variable names are case-insensitive, and a process sees each one
// spelled however its parent's environment block spelled it: the same variable
// arrives as SystemRoot from one parent and SYSTEMROOT from another. An exact
// lookup would scrub a path or keep it depending on who started gc, so the
// lookup ignores case.
func redactionInertEnvKey(key string) bool {
	return envRedactionInertFolded[strings.ToUpper(key)]
}
