//go:build !windows

package config

import (
	"os"
	"reflect"
)

// statCtimeNanos returns the ctime (inode-change time) of info in nanoseconds
// since the epoch, extracted via reflection so the same function handles both
// a real *syscall.Stat_t (whose ctime field is named Ctim on Linux and
// Ctimespec on Darwin/BSD, each holding a nested Sec/Nsec Timespec) and
// fsys.Fake's flat synthetic Ctime field. Returns ok=false when info.Sys()
// exposes none of these shapes.
func statCtimeNanos(info os.FileInfo) (int64, bool) {
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !stat.IsValid() {
		return 0, false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		if field := stat.FieldByName(name); field.IsValid() {
			if nanos, ok := timespecToNanos(field); ok {
				return nanos, true
			}
		}
	}
	if field := stat.FieldByName("Ctime"); field.IsValid() {
		if nanos, ok := int64FieldValue(field); ok {
			return nanos, true
		}
	}
	return 0, false
}

func timespecToNanos(v reflect.Value) (int64, bool) {
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	sec, ok := int64FieldValue(v.FieldByName("Sec"))
	if !ok {
		return 0, false
	}
	nsec, ok := int64FieldValue(v.FieldByName("Nsec"))
	if !ok {
		return 0, false
	}
	return sec*1e9 + nsec, true
}

func int64FieldValue(v reflect.Value) (int64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	default:
		return 0, false
	}
}
