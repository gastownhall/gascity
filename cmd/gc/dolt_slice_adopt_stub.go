//go:build !linux

package main

import "fmt"

func adoptManagedDoltPlacement(string, string) error {
	return fmt.Errorf("managed dolt placement requires Linux cgroup v2 and a systemd user manager")
}
