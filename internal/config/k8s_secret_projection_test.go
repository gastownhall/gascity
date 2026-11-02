package config

import (
	"strings"
	"testing"
)

func TestK8sSecretProjectionParsesAndValidates(t *testing.T) {
	cfg, err := Parse([]byte(`
[session.k8s]
secret_env = [
  { name = "GITHUB_TOKEN", secret = "git-credentials", key = "token" },
  { name = "GITEA_TOKEN", secret = "gitea-credentials", key = "token" },
]
secret_mounts = [
  { secret = "claude-credentials", mount_path = "/tmp/claude-secret" },
]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Session.K8s.SecretEnv) != 2 || cfg.Session.K8s.SecretEnv[1].Name != "GITEA_TOKEN" {
		t.Fatalf("SecretEnv = %#v", cfg.Session.K8s.SecretEnv)
	}
	if len(cfg.Session.K8s.SecretMounts) != 1 || cfg.Session.K8s.SecretMounts[0].MountPath != "/tmp/claude-secret" {
		t.Fatalf("SecretMounts = %#v", cfg.Session.K8s.SecretMounts)
	}
	if err := ValidateK8sSecretProjection(cfg, "city.toml"); err != nil {
		t.Fatalf("ValidateK8sSecretProjection: %v", err)
	}
}

func TestValidateK8sSecretProjectionRejectsMalformedEntries(t *testing.T) {
	tests := []struct {
		name string
		k8s  K8sConfig
		want string
	}{
		{"blank env name", K8sConfig{SecretEnv: []K8sSecretEnv{{Secret: "s", Key: "k"}}}, "non-blank"},
		{"blank env secret", K8sConfig{SecretEnv: []K8sSecretEnv{{Name: "TOKEN", Key: "k"}}}, "non-blank"},
		{"blank env key", K8sConfig{SecretEnv: []K8sSecretEnv{{Name: "TOKEN", Secret: "s"}}}, "non-blank"},
		{"invalid env name", K8sConfig{SecretEnv: []K8sSecretEnv{{Name: "bad-name", Secret: "s", Key: "k"}}}, "environment variable"},
		{"duplicate env name", K8sConfig{SecretEnv: []K8sSecretEnv{{Name: "TOKEN", Secret: "s", Key: "k"}, {Name: "TOKEN", Secret: "s2", Key: "k"}}}, "duplicated"},
		{"blank mount secret", K8sConfig{SecretMounts: []K8sSecretMount{{MountPath: "/secret"}}}, "non-blank"},
		{"blank mount path", K8sConfig{SecretMounts: []K8sSecretMount{{Secret: "s"}}}, "non-blank"},
		{"relative mount path", K8sConfig{SecretMounts: []K8sSecretMount{{Secret: "s", MountPath: "secret"}}}, "absolute"},
		{"duplicate mount path", K8sConfig{SecretMounts: []K8sSecretMount{{Secret: "s", MountPath: "/secret"}, {Secret: "s2", MountPath: "/secret"}}}, "duplicated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateK8sSecretProjection(&City{Session: SessionConfig{K8s: tt.k8s}}, "city.toml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validation error = %v, want text %q", err, tt.want)
			}
		})
	}
}
