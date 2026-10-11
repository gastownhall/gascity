package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyReleaseBinaryMetadata(t *testing.T) {
	const (
		expectedCommit  = "0123456789abcdef0123456789abcdef01234567"
		expectedVersion = "1.2.3"
	)

	cleanBuildInfo := strings.Join([]string{
		"/tmp/gc: go1.26.5",
		"\tpath\tgithub.com/gastownhall/gascity/cmd/gc",
		"\tmod\tgithub.com/gastownhall/gascity\tv1.2.3",
		"\tbuild\tvcs.revision=" + expectedCommit,
		"\tbuild\tvcs.modified=false",
	}, "\n")

	tests := []struct {
		name        string
		versionJSON string
		buildInfo   string
		wantErr     string
	}{
		{
			name:        "clean release",
			versionJSON: `{"commit":"` + expectedCommit + `","version":"1.2.3"}`,
			buildInfo:   cleanBuildInfo,
		},
		{
			name:        "dirty CLI commit",
			versionJSON: `{"commit":"` + expectedCommit + `-dirty","version":"1.2.3"}`,
			buildInfo:   cleanBuildInfo,
			wantErr:     "release binary reports a dirty commit",
		},
		{
			name:        "dirty VCS setting",
			versionJSON: `{"commit":"` + expectedCommit + `","version":"1.2.3"}`,
			buildInfo:   strings.Replace(cleanBuildInfo, "vcs.modified=false", "vcs.modified=true", 1),
			wantErr:     "embedded vcs.modified is true, expected false",
		},
		{
			name:        "missing VCS modified setting",
			versionJSON: `{"commit":"` + expectedCommit + `","version":"1.2.3"}`,
			buildInfo:   strings.Replace(cleanBuildInfo, "\n\tbuild\tvcs.modified=false", "", 1),
			wantErr:     "embedded vcs.modified is missing, expected false",
		},
		{
			name:        "VCS revision mismatch",
			versionJSON: `{"commit":"` + expectedCommit + `","version":"1.2.3"}`,
			buildInfo:   strings.Replace(cleanBuildInfo, expectedCommit, "89abcdef0123456789abcdef0123456789abcdef", 1),
			wantErr:     "embedded vcs.revision is 89abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name:        "missing VCS revision",
			versionJSON: `{"commit":"` + expectedCommit + `","version":"1.2.3"}`,
			buildInfo:   strings.Replace(cleanBuildInfo, "\n\tbuild\tvcs.revision="+expectedCommit, "", 1),
			wantErr:     "embedded vcs.revision is missing",
		},
		{
			name:        "dirty module version",
			versionJSON: `{"commit":"` + expectedCommit + `","version":"1.2.3"}`,
			buildInfo:   strings.Replace(cleanBuildInfo, "v1.2.3", "v1.2.3+dirty", 1),
			wantErr:     "embedded module version is dirty",
		},
		{
			name:        "release version mismatch",
			versionJSON: `{"commit":"` + expectedCommit + `","version":"9.9.9"}`,
			buildInfo:   cleanBuildInfo,
			wantErr:     "release binary version is 9.9.9, expected 1.2.3",
		},
		{
			name:        "missing JSON commit",
			versionJSON: `{"version":"1.2.3"}`,
			buildInfo:   cleanBuildInfo,
			wantErr:     "missing commit",
		},
		{
			name:        "malformed JSON",
			versionJSON: `{"commit":`,
			buildInfo:   cleanBuildInfo,
			wantErr:     "parse error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			binDir := filepath.Join(tmp, "bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatalf("create bin directory: %v", err)
			}

			binary := filepath.Join(tmp, "gc")
			writeExecutable(t, binary, `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$GC_TEST_VERSION_JSON"
`)
			writeExecutable(t, filepath.Join(binDir, "go"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$GC_TEST_BUILD_INFO"
`)

			env := os.Environ()
			env = replaceScriptEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			env = replaceScriptEnv(env, "GC_TEST_VERSION_JSON", tt.versionJSON)
			env = replaceScriptEnv(env, "GC_TEST_BUILD_INFO", tt.buildInfo)

			cmd := scriptCommand(
				repoRoot(t),
				"verify-release-binary-metadata.sh",
				binary,
				expectedCommit,
				expectedVersion,
			)
			cmd.Env = env
			output, err := cmd.CombinedOutput()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verify clean release metadata: %v\n%s", err, output)
				}
				if !strings.Contains(string(output), "release binary metadata: OK") {
					t.Fatalf("success output = %q", output)
				}
				return
			}
			if err == nil {
				t.Fatalf("verification succeeded, want error containing %q\n%s", tt.wantErr, output)
			}
			if !strings.Contains(string(output), tt.wantErr) {
				t.Fatalf("error output = %q, want substring %q", output, tt.wantErr)
			}
		})
	}
}
