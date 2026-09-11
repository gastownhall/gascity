package main

import (
	"go/build"
	"io"
	"runtime"
	"testing"
)

const productMetricsTestRecordHelpCommandFixture = "__testhook-record-help"

func TestProductMetricsBuildTagSurfacesKeepTesthooksOutOfNormalCommandTree(t *testing.T) {
	normal := build.Default
	normal.BuildTags = nil
	tagged := normal
	tagged.BuildTags = []string{"productmetrics_testhook"}

	for _, test := range []struct {
		name         string
		dir          string
		filename     string
		normalSelect bool
		taggedSelect bool
	}{
		{name: "normal census adapter", dir: ".", filename: "metrics_census_ignore_production.go", normalSelect: true},
		{name: "normal metrics adapter", dir: ".", filename: "productmetrics_adapter_production.go", normalSelect: true},
		{name: "normal control registrar", dir: ".", filename: "productmetrics_controls_production.go", normalSelect: true},
		{name: "tagged control registrar", dir: ".", filename: "productmetrics_controls_testhook.go", taggedSelect: true},
		{name: "tagged command adapter", dir: ".", filename: "productmetrics_testhook.go", taggedSelect: true},
		{name: "tagged service adapter", dir: "../../internal/productmetrics", filename: "productmetrics_testhook.go", taggedSelect: true},
		{name: "tagged private process helpers", dir: ".", filename: "productmetrics_private_process_testhook_test.go", taggedSelect: true},
		{name: "tagged Linux PTY helpers", dir: ".", filename: "productmetrics_controls_pty_linux_test.go", taggedSelect: runtime.GOOS == "linux"},
		{name: "tagged Darwin PTY helpers", dir: ".", filename: "productmetrics_controls_pty_darwin_test.go", taggedSelect: runtime.GOOS == "darwin"},
		{
			name:         "tagged process contract",
			dir:          ".",
			filename:     "productmetrics_controls_process_testhook_test.go",
			taggedSelect: runtime.GOOS == "linux" || runtime.GOOS == "darwin",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, surface := range []struct {
				name string
				ctx  build.Context
				want bool
			}{
				{name: "normal", ctx: normal, want: test.normalSelect},
				{name: "tagged", ctx: tagged, want: test.taggedSelect},
			} {
				selected, err := surface.ctx.MatchFile(test.dir, test.filename)
				if err != nil {
					t.Fatalf("%s MatchFile(%q, %q): %v", surface.name, test.dir, test.filename, err)
				}
				if selected != surface.want {
					t.Fatalf("%s MatchFile(%q, %q) = %t, want %t", surface.name, test.dir, test.filename, selected, surface.want)
				}
			}
		})
	}

	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	if _, ok := findCommandByCanonicalPath(root, "gc metrics"); !ok {
		t.Fatal("normal command tree omits gc metrics")
	}
	if _, ok := findCommandByCanonicalPath(root, "gc metrics "+productMetricsTestRecordHelpCommandFixture); ok {
		t.Fatalf("normal command tree exposes tagged command %q", productMetricsTestRecordHelpCommandFixture)
	}
}
