//go:build windows

package builtinpacks

import "os"

// statCtimeNanos is the Windows fallback: Windows exposes no ctime through
// os.FileInfo, so there is nothing to add to the stat signature and
// ok=false leaves it at size and mtime. That is a documented, intentional
// gap, not a silent one: on Windows a same-size rewrite whose mtime is
// restored is still trusted by the validation memo. Unlike
// internal/config/fingerprint_ctime_windows.go, which synthesizes mtime^size
// with ok=true, this reports no ctime at all, so nothing non-discriminating is
// folded into the signature.
func statCtimeNanos(os.FileInfo) (int64, bool) { return 0, false }
