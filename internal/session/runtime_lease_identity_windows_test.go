//go:build windows

package session

import (
	"fmt"
	"testing"
)

func lockFileIdentity(t *testing.T, l *RuntimeLease) string {
	t.Helper()
	fi, err := l.lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	dev, ino, ok := runtimeLeaseFileID(l.lock, fi)
	if !ok {
		t.Fatal("reading the lock file's identity")
	}
	return fmt.Sprintf("%s/%d/%d/%s", runtimeLeaseBootID(), dev, ino, l.token)
}
