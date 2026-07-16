package productmetrics

import (
	"go/build"
	"os"
	"testing"
)

func TestStoragePlatformBuildSelection(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		goos      string
		supported bool
	}{
		{goos: "linux", supported: true},
		{goos: "darwin", supported: true},
		{goos: "android", supported: false},
		{goos: "ios", supported: false},
		{goos: "windows", supported: false},
	} {
		t.Run(test.goos, func(t *testing.T) {
			context := build.Default
			context.GOOS = test.goos
			for _, name := range []string{"storage_unix.go", "lock_unix.go"} {
				matched, err := context.MatchFile(dir, name)
				if err != nil {
					t.Fatal(err)
				}
				if matched != test.supported {
					t.Errorf("%s selected on %s = %v, want %v", name, test.goos, matched, test.supported)
				}
			}
			matched, err := context.MatchFile(dir, "platform_unsupported.go")
			if err != nil {
				t.Fatal(err)
			}
			if matched == test.supported {
				t.Errorf("platform_unsupported.go selected on %s = %v, want %v", test.goos, matched, !test.supported)
			}
		})
	}
}
