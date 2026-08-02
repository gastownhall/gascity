//go:build !darwin

package pidutil

// platformCmdline reports handled=false on platforms without a native argv
// read; Cmdline then falls through to the /proc path.
func platformCmdline(int) (argv []string, err error, handled bool) {
	return nil, nil, false
}
