package packsource

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		input string
		kind  Kind
		reg   string
		pack  string
	}{
		{"registry:main:lighthouse", KindRegistryLocator, "main", "lighthouse"},
		{"registry:main:acme/lighthouse", KindRegistryLocator, "main", "acme/lighthouse"},
		{"main:lighthouse", KindQualifiedName, "main", "lighthouse"},
		{"main:acme/lighthouse", KindQualifiedName, "main", "acme/lighthouse"},
		{"lighthouse", KindBareName, "", "lighthouse"},
		{"./lighthouse", KindPath, "", ""},
		{"../lighthouse", KindPath, "", ""},
		{"https://example.com/lighthouse.git", KindGit, "", ""},
		{"github.com/example/lighthouse", KindGit, "", ""},
		{"git@example.com:team/lighthouse.git", KindGit, "", ""},
	}
	for _, tc := range tests {
		got := Classify(tc.input)
		if got.Kind != tc.kind || got.Registry != tc.reg || got.Pack != tc.pack {
			t.Fatalf("Classify(%q) = %#v, want kind=%s reg=%q pack=%q", tc.input, got, tc.kind, tc.reg, tc.pack)
		}
	}
}

func TestParseRegistryLocatorRejectsInvalid(t *testing.T) {
	for _, input := range []string{
		"main:lighthouse",
		"registry:",
		"registry:Main:lighthouse",
		"registry:main:",
		"registry:main:../lighthouse",
		"registry:main:lighthouse:extra",
	} {
		if _, err := ParseRegistryLocator(input); err == nil {
			t.Fatalf("ParseRegistryLocator(%q) succeeded, want error", input)
		}
	}
}
