//go:build !linux

package events

// renameNoReplace renames oldpath to newpath, failing with an error that
// matches fs.ErrExist instead of replacing an existing newpath. See
// renameIfAbsent for why the non-atomic check is enough.
func renameNoReplace(oldpath, newpath string) error {
	return renameIfAbsent(oldpath, newpath)
}
