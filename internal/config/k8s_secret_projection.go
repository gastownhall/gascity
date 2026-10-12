package config

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var k8sEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateK8sSecretProjection validates the declarative Secret references in
// [session.k8s]. It deliberately does not query Kubernetes or read Secret data.
func ValidateK8sSecretProjection(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	prefix := "[session.k8s]"
	if source != "" {
		prefix = source + ": [session.k8s]"
	}
	envNames := make(map[string]struct{}, len(cfg.Session.K8s.SecretEnv))
	for i, item := range cfg.Session.K8s.SecretEnv {
		field := fmt.Sprintf("%s.secret_env[%d]", prefix, i)
		if strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.Secret) == "" || strings.TrimSpace(item.Key) == "" {
			return fmt.Errorf("%s requires non-blank name, secret, and key", field)
		}
		if !k8sEnvNamePattern.MatchString(item.Name) {
			return fmt.Errorf("%s.name %q is not a valid environment variable name", field, item.Name)
		}
		if _, exists := envNames[item.Name]; exists {
			return fmt.Errorf("%s.name %q is duplicated", field, item.Name)
		}
		envNames[item.Name] = struct{}{}
	}
	mountPaths := make(map[string]struct{}, len(cfg.Session.K8s.SecretMounts))
	for i, item := range cfg.Session.K8s.SecretMounts {
		field := fmt.Sprintf("%s.secret_mounts[%d]", prefix, i)
		if strings.TrimSpace(item.Secret) == "" || strings.TrimSpace(item.MountPath) == "" {
			return fmt.Errorf("%s requires non-blank secret and mount_path", field)
		}
		if !path.IsAbs(item.MountPath) {
			return fmt.Errorf("%s.mount_path %q must be absolute", field, item.MountPath)
		}
		if _, exists := mountPaths[item.MountPath]; exists {
			return fmt.Errorf("%s.mount_path %q is duplicated", field, item.MountPath)
		}
		mountPaths[item.MountPath] = struct{}{}
	}
	return nil
}
