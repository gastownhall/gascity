package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPrePushSuiteLocalRuntimeSelection(t *testing.T) {
	for _, tc := range []struct {
		name, mode, probe, announce string
		noBazel, noGo, stalePin     bool
		infoExit                    string
		wantCode                    int
		wantBazel, wantMake         string
		wantProbe                   bool
		wantText                    []string
	}{
		{
			name: "missing ICU uses Go", probe: "missing libicui18n.so.74 libicuuc.so.74 libicudata.so.74", wantProbe: true, wantMake: prePushMakeArgs,
			wantText: []string{"libicui18n.so.74 libicuuc.so.74 libicudata.so.74", "MODULE.bazel", "remote executor", "libicu74", "bazel --batch", "GC_PREPUSH_SUITE=cache"},
		},
		{name: "resolvable ICU preserves cache", probe: "ok", wantProbe: true, wantBazel: prePushBazelCacheArgs},
		{name: "other OS keeps cache", probe: "n/a Darwin/arm64", wantProbe: true, wantBazel: prePushBazelCacheArgs},
		{name: "other Linux architecture keeps cache", probe: "n/a Linux/aarch64", wantProbe: true, wantBazel: prePushBazelCacheArgs},
		{
			name: "unavailable loader evidence uses Go", probe: "unknown loader did not report preload failures", wantProbe: true, wantMake: prePushMakeArgs,
			wantText: []string{"could not verify", "loader did not report preload failures", "GC_PREPUSH_SUITE=cache"},
		},
		{name: "empty result uses Go", probe: "", wantProbe: true, wantMake: prePushMakeArgs, wantText: []string{"could not verify", "probe produced no usable result"}},
		{name: "malformed result uses Go", probe: "ready maybe", wantProbe: true, wantMake: prePushMakeArgs, wantText: []string{"could not verify", "probe produced no usable result"}},
		{name: "multiple result lines use Go", probe: "ok\nmissing libicuuc.so.74", wantProbe: true, wantMake: prePushMakeArgs, wantText: []string{"could not verify", "probe produced no usable result"}},
		{name: "remote executor needs no local runtime", probe: "missing libicuuc.so.74", announce: announceHome, wantBazel: prePushBazelRBEArgs},
		{
			name: "stale remote pin checks local runtime", probe: "missing libicuuc.so.74", announce: announceHome, stalePin: true, wantProbe: true, wantMake: prePushMakeArgs,
			wantText: []string{"not executing remotely", "Rebase onto main", "libicuuc.so.74"},
		},
		{name: "no bazel needs no runtime probe", probe: "missing libicuuc.so.74", noBazel: true, wantMake: prePushMakeArgs, wantText: []string{"bazel is not installed"}},
		{name: "missing pinned Go is decided first", probe: "missing libicuuc.so.74", noGo: true, wantMake: prePushMakeArgs, wantText: []string{"no go on .bazelrc's pinned test PATH"}},
		{name: "unreadable executor options stay fatal", probe: "missing libicuuc.so.74", infoExit: "2", wantCode: 2, wantText: []string{"could not read"}},
		{name: "forced cache ignores missing ICU", mode: "cache", probe: "missing libicuuc.so.74", wantBazel: prePushBazelCacheArgs},
		{name: "forced rbe ignores missing ICU", mode: "rbe", probe: "missing libicuuc.so.74", announce: announceHome, wantBazel: prePushBazelRBEArgs},
		{name: "forced rbe retains pin refusal", mode: "rbe", probe: "missing libicuuc.so.74", announce: announceHome, stalePin: true, wantCode: 2, wantText: []string{"GC_PREPUSH_SUITE=rbe refused"}},
		{name: "forced Go ignores missing ICU", mode: "go", probe: "missing libicuuc.so.74", wantMake: prePushMakeArgs, wantText: []string{"why: GC_PREPUSH_SUITE=go"}},
		{name: "unknown mode stays fatal", mode: "skip", probe: "missing libicuuc.so.74", wantCode: 2, wantText: []string{"GC_PREPUSH_SUITE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			record := filepath.Join(t.TempDir(), "runtime-probes")
			installLocalRuntimeProbe(t, repoRoot(t), f.repo, tc.probe, record)
			var runs string
			if !tc.noBazel {
				announce := tc.announce
				if announce == "" {
					announce = announceNone
				}
				runs, _ = f.withFakeBazel(t, announce)
			}
			if tc.stalePin {
				f.setMainPin(t, f.workerEnv, prePushPinOther)
			}
			if tc.noGo {
				writeDriftFile(t, filepath.Join(f.repo, ".bazelrc"), "test --test_env=PATH=/absent/pinned/go\n")
			}
			f.env = append(f.env, "GC_PREPUSH_SUITE="+tc.mode, "BAZEL_INFO_EXIT="+tc.infoExit)
			code, out := f.run(t, f.pushRefLine())
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d\n%s", code, tc.wantCode, out)
			}
			if got := strings.TrimSpace(f.read(t, f.makeRuns)); got != tc.wantMake {
				t.Errorf("make = %q, want %q\n%s", got, tc.wantMake, out)
			}
			if runs != "" {
				if got := strings.TrimSpace(f.read(t, runs)); got != tc.wantBazel {
					t.Errorf("bazel test = %q, want %q\n%s", got, tc.wantBazel, out)
				}
			}
			wantCalls := ""
			if tc.wantProbe {
				wantCalls = "probe\n"
			}
			if got := f.read(t, record); got != wantCalls {
				t.Errorf("runtime probe calls = %q, want %q", got, wantCalls)
			}
			assertGoFallbackAnnounced(t, out, tc.wantMake != "", "")
			for _, want := range tc.wantText {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}
