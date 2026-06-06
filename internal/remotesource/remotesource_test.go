package remotesource

import "testing"

func TestParseSourceForms(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		cloneURL  string
		subpath   string
		ref       string
		githubMod string
	}{
		{
			name:     "canonical subpath ref",
			source:   "https://github.com/gastownhall/gascity.git//internal/bootstrap/packs/core#main",
			cloneURL: "https://github.com/gastownhall/gascity.git",
			subpath:  "internal/bootstrap/packs/core",
			ref:      "main",
		},
		{
			name:     "short github",
			source:   "github.com/gastownhall/gascity//examples/bd",
			cloneURL: "https://github.com/gastownhall/gascity",
			subpath:  "examples/bd",
		},
		{
			name:     "no git suffix",
			source:   "https://github.com/gastownhall/gascity//examples/dolt",
			cloneURL: "https://github.com/gastownhall/gascity",
			subpath:  "examples/dolt",
		},
		{
			name:      "github tree",
			source:    "https://github.com/gastownhall/gascity/tree/release/examples/gastown/packs/maintenance",
			cloneURL:  "https://github.com/gastownhall/gascity.git",
			subpath:   "examples/gastown/packs/maintenance",
			ref:       "release",
			githubMod: "tree",
		},
		{
			name:      "github blob pack toml",
			source:    "https://github.com/gastownhall/gascity/blob/release/examples/gastown/packs/gastown/pack.toml",
			cloneURL:  "https://github.com/gastownhall/gascity.git",
			subpath:   "examples/gastown/packs/gastown",
			ref:       "release",
			githubMod: "blob",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.source)
			if got.CloneURL != tt.cloneURL {
				t.Fatalf("CloneURL = %q, want %q", got.CloneURL, tt.cloneURL)
			}
			if got.Subpath != tt.subpath {
				t.Fatalf("Subpath = %q, want %q", got.Subpath, tt.subpath)
			}
			if got.Ref != tt.ref {
				t.Fatalf("Ref = %q, want %q", got.Ref, tt.ref)
			}
			if got.GitHubMode != tt.githubMod {
				t.Fatalf("GitHubMode = %q, want %q", got.GitHubMode, tt.githubMod)
			}
		})
	}
}

func TestFormatGitHubTreeSource(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		ref     string
		subpath string
		want    string
		ok      bool
	}{
		{
			name:    "https git suffix",
			source:  "https://github.com/gastownhall/gascity-packs.git",
			ref:     "main",
			subpath: "flywheel/cass",
			want:    "https://github.com/gastownhall/gascity-packs/tree/main/flywheel/cass",
			ok:      true,
		},
		{
			name:    "bare github",
			source:  "github.com/gastownhall/gascity-packs",
			ref:     "v1.2.3",
			subpath: "/gascity/",
			want:    "https://github.com/gastownhall/gascity-packs/tree/v1.2.3/gascity",
			ok:      true,
		},
		{
			name:    "slash ref stays legacy",
			source:  "https://github.com/gastownhall/gascity-packs.git",
			ref:     "design/gc-plan-pack",
			subpath: "gc",
			ok:      false,
		},
		{
			name:    "ssh stays legacy",
			source:  "git@github.com:gastownhall/gascity-packs.git",
			ref:     "main",
			subpath: "gascity",
			ok:      false,
		},
		{
			name:    "missing ref stays legacy",
			source:  "https://github.com/gastownhall/gascity-packs.git",
			subpath: "gascity",
			ok:      false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FormatGitHubTreeSource(tt.source, tt.ref, tt.subpath)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseGitHubTreeOrBlobRejectsNonGitHubTreeBlob(t *testing.T) {
	if _, ok := ParseGitHubTreeOrBlob("https://github.com/org/repo/issues/1"); ok {
		t.Fatal("ParseGitHubTreeOrBlob accepted issue URL")
	}
	if _, ok := ParseGitHubTreeOrBlob("https://example.com/org/repo/tree/main/pack"); ok {
		t.Fatal("ParseGitHubTreeOrBlob accepted non-GitHub URL")
	}
}

func TestIsRemote(t *testing.T) {
	for _, source := range []string{
		"git@github.com:org/repo.git",
		"ssh://git@example.com/org/repo.git",
		"https://example.com/org/repo.git",
		"http://example.com/org/repo.git",
		"file:///tmp/repo.git",
		"github.com/org/repo",
	} {
		if !IsRemote(source) {
			t.Fatalf("IsRemote(%q) = false, want true", source)
		}
	}
	if IsRemote("./packs/local") {
		t.Fatal("IsRemote accepted local path")
	}
}
