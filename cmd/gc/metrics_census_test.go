package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestForcedCobraDefaultsPreserveBaselineOutput(t *testing.T) {
	t.Run("completion extra", func(t *testing.T) {
		configureIsolatedRuntimeEnv(t)
		var stdout, stderr bytes.Buffer
		if code := run([]string{"completion", "bash", "extra"}, &stdout, &stderr); code != 1 {
			t.Fatalf("run = %d, want 1", code)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("stdout=%q stderr=%q, want both empty", stdout.String(), stderr.String())
		}
	})

	t.Run("completion unknown shell", func(t *testing.T) {
		configureIsolatedRuntimeEnv(t)
		var wantStdout, wantStderr bytes.Buffer
		if code := run([]string{"completion"}, &wantStdout, &wantStderr); code != 0 || wantStderr.Len() != 0 {
			t.Fatalf("baseline completion: code=%d stderr=%q", code, wantStderr.String())
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"completion", "bogus"}, &stdout, &stderr); code != 0 {
			t.Fatalf("run = %d, want 0", code)
		}
		if stdout.String() != wantStdout.String() || stderr.Len() != 0 {
			t.Fatalf("output drift: stdout_equal=%t stderr=%q", stdout.String() == wantStdout.String(), stderr.String())
		}
	})

	t.Run("help unknown target", func(t *testing.T) {
		configureIsolatedRuntimeEnv(t)
		var wantStdout, wantStderr bytes.Buffer
		if code := run(nil, &wantStdout, &wantStderr); code != 0 || wantStderr.Len() != 0 {
			t.Fatalf("baseline root help: code=%d stderr=%q", code, wantStderr.String())
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"help", "extra"}, &stdout, &stderr); code != 0 {
			t.Fatalf("run = %d, want 0", code)
		}
		if stdout.String() != wantStdout.String() || stderr.Len() != 0 {
			t.Fatalf("output drift: stdout_equal=%t stderr=%q", stdout.String() == wantStdout.String(), stderr.String())
		}
	})
}

func TestProductMetricsCommandCensusMatchesProductionBuiltins(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	if err := validateProductMetricsCommandCensus(root, generatedProductMetricsCommandCensus); err != nil {
		t.Fatal(err)
	}
}

func TestProductMetricsCommandCensusRejectsStructuralDrift(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	if len(generatedProductMetricsCommandCensus) == 0 {
		t.Fatal("generated product-metrics census is empty")
	}

	missing := append([]productMetricsCommandCensusEntry(nil), generatedProductMetricsCommandCensus...)
	missing = missing[1:]
	if err := validateProductMetricsCommandCensus(root, missing); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing row error = %v", err)
	}

	root.AddCommand(&cobra.Command{Use: "uncensused", Run: func(*cobra.Command, []string) {}})
	if err := validateProductMetricsCommandCensus(root, generatedProductMetricsCommandCensus); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("new live node error = %v", err)
	}
}

func TestProductMetricsCommandCensusRejectsAliasAndHiddenDrift(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	citiesList, ok := findCommandByCanonicalPath(root, "gc cities list")
	if !ok {
		t.Fatal("missing gc cities list")
	}
	citiesList.Aliases = nil
	if err := validateProductMetricsCommandCensus(root, generatedProductMetricsCommandCensus); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Fatalf("alias drift error = %v", err)
	}

	root = newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	workflowChild, ok := findCommandByCanonicalPath(root, "gc workflow control")
	if !ok {
		t.Fatal("missing gc workflow control")
	}
	workflowChild.Parent().Hidden = false
	if err := validateProductMetricsCommandCensus(root, generatedProductMetricsCommandCensus); err == nil || !strings.Contains(err.Error(), "hidden state") {
		t.Fatalf("effective hidden drift error = %v", err)
	}
}

func TestProductMetricsCommandCensusRejectsSiblingAliasCollision(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(
		&cobra.Command{Use: "one", Aliases: []string{"shared"}},
		&cobra.Command{Use: "two", Aliases: []string{"shared"}},
	)
	if err := validateProductMetricsCommandCensus(root, generatedProductMetricsCommandCensus); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestProductMetricsCensusMismatchClonesAnnotationsAndLeavesNoPartialState(t *testing.T) {
	shared := map[string]string{
		"unrelated":                       "keep",
		productMetricsClassAnnotation:     packCommandClassificationValue,
		productMetricsIDAnnotation:        "99",
		productMetricsModeAnnotation:      "stale",
		productMetricsNoticeAnnotation:    "stale",
		productMetricsRecordingAnnotation: "stale",
		productMetricsOwnerAnnotation:     "stale",
		productMetricsResolverAnnotation:  "stale",
	}
	root := &cobra.Command{Use: "gc", Annotations: shared}
	child := &cobra.Command{Use: "dynamic", Annotations: shared, Run: func(*cobra.Command, []string) {}}
	root.AddCommand(child)

	applyProductionProductMetricsCommandCensus(root)

	if fmt.Sprintf("%p", root.Annotations) == fmt.Sprintf("%p", child.Annotations) {
		t.Fatal("commands still share one annotations map")
	}
	for _, command := range []*cobra.Command{root, child} {
		if command.Annotations["unrelated"] != "keep" || command.Annotations[productMetricsClassAnnotation] != packCommandClassificationValue {
			t.Fatalf("preserved annotations changed: %#v", command.Annotations)
		}
		for _, key := range []string{productMetricsIDAnnotation, productMetricsModeAnnotation, productMetricsNoticeAnnotation, productMetricsRecordingAnnotation, productMetricsOwnerAnnotation, productMetricsResolverAnnotation, productMetricsExclusionAnnotation, productMetricsCensusValidAnnotation} {
			if _, exists := command.Annotations[key]; exists {
				t.Fatalf("stale product-metrics annotation %q remains on %s", key, command.CommandPath())
			}
		}
	}
}

func TestClassifyProductMetricsCommandRejectsUnknownNestedCompletion(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	got := classifyProductMetricsCommand(root, []string{"completion", "bogus"}, productMetricsPolicyContext{})
	if got.ID != productMetricsCommandUnknown {
		t.Fatalf("classification = %+v, want unknown", got)
	}
}

func TestProductMetricsCommandCensusIncludesForcedCobraDefaults(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	for _, path := range []string{"gc help", "gc completion", "gc completion bash"} {
		if _, ok := findCommandByCanonicalPath(root, path); !ok {
			t.Fatalf("built-in tree is missing forced Cobra node %q", path)
		}
	}
}
