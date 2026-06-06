package main

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

func waitForProcessGroupExit(pgid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !processGroupAlive(pgid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process group %d did not exit within %s", pgid, timeout)
		}
		time.Sleep(supervisorProcessGroupPollPeriod)
	}
}

func processGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := supervisorKill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
