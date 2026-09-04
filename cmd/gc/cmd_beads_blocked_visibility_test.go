package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBeadsListHelpDescribesDefaultAsAllNonclosedStatuses(t *testing.T) {
	t.Parallel()

	command, _, err := newBeadsCmd(&bytes.Buffer{}, &bytes.Buffer{}).Find([]string{"list"})
	if err != nil {
		t.Fatalf("Find(list): %v", err)
	}
	usage := command.Flag("all").Usage
	if !strings.Contains(usage, "all nonclosed statuses") {
		t.Fatalf("--all usage = %q, want default all nonclosed statuses", usage)
	}
}
