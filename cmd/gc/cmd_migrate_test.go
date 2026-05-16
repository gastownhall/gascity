package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDoImportMigrateIsDeprecatedShim(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := doImportMigrate(false, &stdout, &stderr); code != 1 {
		t.Fatalf("doImportMigrate = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	for _, want := range []string{
		"gc import migrate has been deprecated",
		`Use "gc doctor" to inspect legacy PackV1 surfaces.`,
		`Use "gc doctor --fix" for the safe mechanical cases that currently have automatic rewrites`,
		"in-place PackV1-to-PackV2 rewrites",
		"docs/guides/migrating-to-pack-vnext.md",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestDoImportMigrateDryRunUsesSameGuidance(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := doImportMigrate(true, &stdout, &stderr); code != 1 {
		t.Fatalf("doImportMigrate(dry-run) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), `gc doctor --fix`) {
		t.Fatalf("stderr missing gc doctor guidance:\n%s", stderr.String())
	}
}
