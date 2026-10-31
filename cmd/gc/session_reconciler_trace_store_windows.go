//go:build windows

package main

import "golang.org/x/sys/windows"

func traceAvailableBytes(path string) (uint64, error) {
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(path), &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}
