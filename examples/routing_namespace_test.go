package examples_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShippedExamplesDoNotHardcodeShortRoutedToPools(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	root := filepath.Dir(filename)
	badRoutes := []string{
		"gc.routed_to=dog",
		"gc.routed_to=worker",
		"gc.routed_to=<rig>/polecat",
		"gc.routed_to=<rig>/refinery",
		"gc.routed_to={{ .RigName }}/refinery",
	}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(data)
		for _, bad := range badRoutes {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains short-form routed_to target %q", path, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
