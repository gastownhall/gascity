//go:build !windows

package builtinpacks

import (
	"os"
	"reflect"
)

// statCtimeNanos returns the ctime (inode-change time) of info in nanoseconds
// since the epoch. The field holding it is named Ctim on Linux and Ctimespec on
// Darwin/BSD, each a nested Sec/Nsec timespec, so it is read by reflection over
// info.Sys() rather than by a *syscall.Stat_t assertion that would not compile
// on every unix. Returns ok=false when info.Sys() exposes neither shape.
func statCtimeNanos(info os.FileInfo) (int64, bool) {
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !stat.IsValid() || stat.Kind() != reflect.Struct {
		return 0, false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		ts := stat.FieldByName(name)
		if !ts.IsValid() || ts.Kind() != reflect.Struct {
			continue
		}
		sec, nsec := ts.FieldByName("Sec"), ts.FieldByName("Nsec")
		if sec.IsValid() && nsec.IsValid() && sec.CanInt() && nsec.CanInt() {
			return sec.Int()*1e9 + nsec.Int(), true
		}
	}
	return 0, false
}
