package citylayout

import "testing"

func TestPackRuntimeEnv(t *testing.T) {
	cityRoot := "/city"

	got := PackRuntimeEnv(cityRoot, "rlm")
	want := map[string]string{
		"GC_CITY":             cityRoot,
		"GC_CITY_PATH":        cityRoot,
		"GC_CITY_RUNTIME_DIR": "/city/.gc/runtime",
		"GC_PACK_STATE_DIR":   "/city/.gc/runtime/packs/rlm",
	}

	lookup := make(map[string]string, len(got))
	for _, entry := range got {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				lookup[entry[:i]] = entry[i+1:]
				break
			}
		}
	}

	for key, expected := range want {
		if lookup[key] != expected {
			t.Fatalf("%s = %q, want %q", key, lookup[key], expected)
		}
	}
}

func TestPackRuntimeEnvMapWithoutPackName(t *testing.T) {
	got := PackRuntimeEnvMap("/city", "")
	if got["GC_CITY_RUNTIME_DIR"] != "/city/.gc/runtime" {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q", got["GC_CITY_RUNTIME_DIR"], "/city/.gc/runtime")
	}
	if _, ok := got["GC_PACK_STATE_DIR"]; ok {
		t.Fatal("GC_PACK_STATE_DIR should be omitted when pack name is empty")
	}
}

func TestPublishedServicesDir(t *testing.T) {
	if got := PublishedServicesDir("/city"); got != "/city/.gc/services/.published" {
		t.Fatalf("PublishedServicesDir = %q, want %q", got, "/city/.gc/services/.published")
	}
}

func TestSessionNameLocksDir(t *testing.T) {
	if got := SessionNameLocksDir("/city"); got != "/city/.gc/session-name-locks" {
		t.Fatalf("SessionNameLocksDir = %q, want %q", got, "/city/.gc/session-name-locks")
	}
}
