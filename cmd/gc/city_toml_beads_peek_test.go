package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeBeadsPeekCityTOML(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCityTOMLBeadsPeekDecodesOncePerInvocation pins the peek's cost inside a
// one-shot command. peekBeadsProvider and peekBeadsBackend read one table of
// city.toml, but decoding TOML means decoding the whole file: about 15 ms for a
// production-sized city.toml, paid 10 times by one `gc bd show` and 19 times by
// one `gc ready` on maintainer-city. Inside an invocation memo scope the file is
// decoded once.
func TestCityTOMLBeadsPeekDecodesOncePerInvocation(t *testing.T) {
	path := writeBeadsPeekCityTOML(t, t.TempDir(), "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"bd\"\nbackend = \"doltlite\"\n")

	t.Run("inside a scope", func(t *testing.T) {
		end := beginInvocationConfigMemo()
		defer end()
		before := cityTOMLBeadsPeekDecodes.Load()
		for range 3 {
			if got := peekBeadsProvider(path); got != "bd" {
				t.Fatalf("peekBeadsProvider = %q, want bd", got)
			}
			if got := peekBeadsBackend(path); got != "doltlite" {
				t.Fatalf("peekBeadsBackend = %q, want doltlite", got)
			}
		}
		if got := cityTOMLBeadsPeekDecodes.Load() - before; got != 1 {
			t.Fatalf("city.toml decodes = %d, want 1 per scope", got)
		}
	})

	t.Run("outside a scope every peek reads the file", func(t *testing.T) {
		before := cityTOMLBeadsPeekDecodes.Load()
		peekBeadsProvider(path)
		peekBeadsBackend(path)
		if got := cityTOMLBeadsPeekDecodes.Load() - before; got != 2 {
			t.Fatalf("city.toml decodes = %d, want 2: a long-lived process must see edits", got)
		}
	})
}

// TestCityTOMLBeadsPeekDoesNotRememberAFailedRead pins that only a decoded
// answer is shared: a peek that could not read or parse the file is retried by
// the next caller, the same rule the composed-config memo keeps for errors.
func TestCityTOMLBeadsPeekDoesNotRememberAFailedRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	end := beginInvocationConfigMemo()
	defer end()
	if got := peekBeadsProvider(path); got != "" {
		t.Fatalf("peekBeadsProvider(missing) = %q, want empty", got)
	}
	writeBeadsPeekCityTOML(t, dir, "[beads]\nprovider = \"file\"\n")
	if got := peekBeadsProvider(path); got != "file" {
		t.Fatalf("peekBeadsProvider after the file appeared = %q, want file", got)
	}
	other := filepath.Join(t.TempDir(), "city.toml")
	writeBeadsPeekCityTOML(t, filepath.Dir(other), "[beads\nprovider = broken")
	if got := peekBeadsProvider(other); got != "" {
		t.Fatalf("peekBeadsProvider(unparsable) = %q, want empty", got)
	}
	writeBeadsPeekCityTOML(t, filepath.Dir(other), "[beads]\nprovider = \"bd\"\n")
	if got := peekBeadsProvider(other); got != "bd" {
		t.Fatalf("peekBeadsProvider after the file was fixed = %q, want bd", got)
	}
}

// TestGcBdPassthroughDecodesCityTOMLOnce pins the passthrough end to end: a
// `gc bd show` decodes city.toml for its [beads] peeks once, however many
// guards and env builders ask.
func TestGcBdPassthroughDecodesCityTOMLOnce(t *testing.T) {
	newSingleExecTestCity(t)
	before := cityTOMLBeadsPeekDecodes.Load()
	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"show", "demo-abc", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(show) = %d, want 0; stderr=%q", got, stderr.String())
	}
	if got := cityTOMLBeadsPeekDecodes.Load() - before; got != 1 {
		t.Fatalf("city.toml [beads] decodes = %d, want 1", got)
	}
}
