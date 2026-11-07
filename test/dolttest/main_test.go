package dolttest

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := ArmOwnerReaper(); err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:errcheck
		os.Exit(1)
	}
	if os.Getenv(ownerHelperEnv) == "1" {
		os.Exit(runOwnerHelper())
	}
	os.Exit(m.Run())
}
