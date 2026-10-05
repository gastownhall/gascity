package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreBDVersionParsesTheReleaseToken(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("bd version 1.3.2-rc.1 (3fdced064: HEAD@3fdced064c73)\n"), nil
	}
	got, err := beads.NewBdStore("/city", runner).BDVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.3.2-rc.1" {
		t.Errorf("BDVersion = %q, want 1.3.2-rc.1", got)
	}
	if strings.Join(gotArgs, " ") != "version" {
		t.Errorf("args = %q, want version", strings.Join(gotArgs, " "))
	}
}

func TestBdStoreBDVersionRejectsUnparseableOutput(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) { return []byte("garbage"), nil }
	if _, err := beads.NewBdStore("/city", runner).BDVersion(); err == nil {
		t.Fatal("expected an error for output with no version token")
	}
}

func TestBdStoreConfigGetReadsTheValue(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"key":"custom.k","schema_version":1,"value":"1.3.1"}`), nil
	}
	got, err := beads.NewBdStore("/city", runner).ConfigGet("custom.k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.3.1" {
		t.Errorf("ConfigGet = %q, want 1.3.1", got)
	}
	if strings.Join(gotArgs, " ") != "config get --json custom.k" {
		t.Errorf("args = %q", strings.Join(gotArgs, " "))
	}
}

func TestBdStoreConfigGetUnsetKeyIsEmpty(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`{"key":"custom.k","schema_version":1,"value":""}`), nil
	}
	got, err := beads.NewBdStore("/city", runner).ConfigGet("custom.k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("ConfigGet = %q, want empty", got)
	}
}

func TestBdStoreConfigGetErrors(t *testing.T) {
	for name, runner := range map[string]beads.CommandRunner{
		"exec":      func(_, _ string, _ ...string) ([]byte, error) { return nil, errors.New("exit status 1") },
		"malformed": func(_, _ string, _ ...string) ([]byte, error) { return []byte("not json"), nil },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := beads.NewBdStore("/city", runner).ConfigGet("custom.k")
			if err == nil || !strings.Contains(err.Error(), "bd config get") {
				t.Fatalf("err = %v, want a bd config get error", err)
			}
		})
	}
}

func TestBdStoreRecomputeBlockedReportsRowsCorrected(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("{\n  \"rows_corrected\": 2,\n  \"schema_version\": 1\n}\n"), nil
	}
	got, err := beads.NewBdStore("/city", runner).RecomputeBlocked()
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("RecomputeBlocked = %d, want 2", got)
	}
	if strings.Join(gotArgs, " ") != "recompute-blocked --json" {
		t.Errorf("args = %q", strings.Join(gotArgs, " "))
	}
}

func TestBdStoreRecomputeBlockedErrors(t *testing.T) {
	for name, runner := range map[string]beads.CommandRunner{
		"exec":          func(_, _ string, _ ...string) ([]byte, error) { return nil, errors.New("exit status 1") },
		"malformed":     func(_, _ string, _ ...string) ([]byte, error) { return []byte("nope"), nil },
		"missing-count": func(_, _ string, _ ...string) ([]byte, error) { return []byte(`{"schema_version":1}`), nil },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := beads.NewBdStore("/city", runner).RecomputeBlocked()
			if err == nil || !strings.Contains(err.Error(), "bd recompute-blocked") {
				t.Fatalf("err = %v, want a bd recompute-blocked error", err)
			}
		})
	}
}
