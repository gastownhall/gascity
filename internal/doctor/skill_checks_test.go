package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeSkillMD creates a skill directory at <skillsDir>/<name>/ with a
// minimal SKILL.md file so validation.listAgentLocalSkills sees it.
func writeSkillMD(t *testing.T, skillsDir, name string) { //nolint:unparam // name currently always "plan"; keep the flexible signature for future tests
	t.Helper()
	dir := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# skill"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// mkSkillsDir returns tmp/<agent>/skills after creating it on disk.
func mkSkillsDir(t *testing.T, tmp, agent string) string {
	t.Helper()
	dir := filepath.Join(tmp, agent, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return dir
}

func TestSkillCollisionCheck_NoCollisions(t *testing.T) {
	tmp := t.TempDir()
	aSkills := mkSkillsDir(t, tmp, "mayor")
	writeSkillMD(t, aSkills, "plan")

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", Provider: "claude", Scope: "city", SkillsDir: aSkills},
		},
	}

	chk := NewSkillCollisionCheck(cfg, tmp)
	res := chk.Run(&CheckContext{CityPath: tmp})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg=%q", res.Status, res.Message)
	}
}

func TestSkillCollisionCheck_CityCollisionMessage(t *testing.T) {
	tmp := t.TempDir()
	mayorSkills := mkSkillsDir(t, tmp, "mayor")
	supervisorSkills := mkSkillsDir(t, tmp, "supervisor")
	writeSkillMD(t, mayorSkills, "plan")
	writeSkillMD(t, supervisorSkills, "plan")

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", Provider: "claude", Scope: "city", SkillsDir: mayorSkills},
			{Name: "supervisor", Provider: "claude", Scope: "city", SkillsDir: supervisorSkills},
		},
	}

	chk := NewSkillCollisionCheck(cfg, "/path/to/city")
	res := chk.Run(&CheckContext{CityPath: "/path/to/city"})
	if res.Status != StatusError {
		t.Fatalf("status = %v, want Error; msg=%q", res.Status, res.Message)
	}

	msg := res.Message
	wantSubstrings := []string{
		"agent-local skill collision at scope root /path/to/city (claude)",
		`"plan" is provided by both mayor and supervisor`,
		"rename one of the colliding skills to resolve",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(msg, s) {
			t.Errorf("message missing %q\nfull message:\n%s", s, msg)
		}
	}
	if res.FixHint == "" {
		t.Errorf("FixHint should be set")
	}
	if chk.CanFix() {
		t.Errorf("CanFix should be false")
	}
	if err := chk.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix should be a no-op, got %v", err)
	}
}

func TestSkillCollisionCheck_RigCollisionUsesRigPath(t *testing.T) {
	tmp := t.TempDir()
	aSkills := mkSkillsDir(t, tmp, "a")
	bSkills := mkSkillsDir(t, tmp, "b")
	writeSkillMD(t, aSkills, "plan")
	writeSkillMD(t, bSkills, "plan")

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "pit", Path: "/path/to/rig"}},
		Agents: []config.Agent{
			{Name: "a", Provider: "claude", Scope: "rig", Dir: "pit", SkillsDir: aSkills},
			{Name: "b", Provider: "claude", Scope: "rig", Dir: "pit", SkillsDir: bSkills},
		},
	}

	chk := NewSkillCollisionCheck(cfg, "/path/to/city")
	res := chk.Run(&CheckContext{CityPath: "/path/to/city"})
	if res.Status != StatusError {
		t.Fatalf("status = %v, want Error; msg=%q", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "scope root /path/to/rig (claude)") {
		t.Errorf("message should include rig path, got:\n%s", res.Message)
	}
	if !strings.Contains(res.Message, `"plan" is provided by both pit/a and pit/b`) {
		t.Errorf("message should include qualified agent names, got:\n%s", res.Message)
	}
}

func TestSkillCollisionCheck_NilCfg(t *testing.T) {
	chk := NewSkillCollisionCheck(nil, "")
	res := chk.Run(&CheckContext{})
	if res.Status != StatusOK {
		t.Fatalf("nil cfg should yield OK, got %v (msg=%q)", res.Status, res.Message)
	}
}
