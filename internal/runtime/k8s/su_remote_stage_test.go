package k8s

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// runUnderEnvClearingSu runs the su part of a pod command (from "CMD=$(") in
// a shell whose environment carries the pod spec's variables, with fakes for
// su (which, like `su -`, clears the environment), tmux (which records the
// environment its server would start with) and sleep. It returns what tmux
// saw.
func runUnderEnvClearingSu(t *testing.T, command string, podEnv map[string]string) string {
	t.Helper()
	at := strings.Index(command, "CMD=$(")
	if at < 0 {
		t.Fatalf("no agent launch in %q", command)
	}
	dir := t.TempDir()
	envOut := filepath.Join(dir, "tmux.env")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// su - <user> -c <cmd>: a login shell with a cleared environment. cd is
	// stubbed: the pod's /workspace does not exist on the test host.
	write("su", "#!/bin/sh\nexec env -i PATH=\"$PATH\" /bin/sh -c \"cd() { :; }; $4\"\n")
	write("tmux", "#!/bin/sh\nenv > "+envOut+"\n")
	write("sleep", "#!/bin/sh\nexit 0\n")
	cmd := exec.Command("/bin/sh", "-c", command[at:])
	cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
	for k, v := range podEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the launch: %v\n%s", err, out)
	}
	got, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("tmux never ran: %v", err)
	}
	return string(got)
}

// TestLinuxUsernameAgentCarriesTheRemoteStageEnv: with LINUX_USERNAME the
// agent's tmux starts through `su -`, a login shell that clears the
// environment. The remote-stage variables must still reach it (and its tmux
// server), in the entrypoint and in a respawn, prebaked or not, whatever
// characters the values hold.
func TestLinuxUsernameAgentCarriesTheRemoteStageEnv(t *testing.T) {
	const ctrlCity = `/srv/my city/$HOME"x'`
	p := newProviderWithOps(newFakeK8sOps())
	p.cityName = `bright "lights"`
	cfg := runtime.Config{
		Command: "claude",
		WorkDir: ctrlCity,
		Env:     map[string]string{"GC_AGENT": "mayor", "GC_CITY": ctrlCity, "LINUX_USERNAME": "alice"},
	}
	want := func(t *testing.T, env string) {
		t.Helper()
		for _, line := range []string{remoteStageControllerEnv + "=" + ctrlCity, remoteStageCityEnv + "=" + p.cityName} {
			if !strings.Contains(env, line+"\n") {
				t.Errorf("the agent's environment lacks %q:\n%s", line, env)
			}
		}
	}
	podEnv := map[string]string{remoteStageControllerEnv: ctrlCity, remoteStageCityEnv: p.cityName}
	for _, prebaked := range []bool{false, true} {
		p.prebaked = prebaked
		pod, err := buildPod("gc-test-agent", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		t.Run("entrypoint", func(t *testing.T) {
			want(t, runUnderEnvClearingSu(t, pod.Spec.Containers[0].Args[0], podEnv))
		})
	}
	t.Run("respawn", func(t *testing.T) {
		want(t, runUnderEnvClearingSu(t, buildRespawnCommand(cfg, p.cityName), podEnv))
	})
}
