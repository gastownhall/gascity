package runtime

import "testing"

// TestProgramStatusArmExcludedFromFingerprint pins design 8.4: the capture
// declaration is leaf-applied, so arming, disarming or changing it never moves
// any fingerprint and never relaunches or restarts the agent.
func TestProgramStatusArmExcludedFromFingerprint(t *testing.T) {
	base := Config{Command: "agent", Env: map[string]string{"A": "1"}, OperatorEnv: map[string]string{"A": "1"}}
	armed := base
	armed.ProgramStatus = ProgramStatusArm{Capture: true, App: "agent", Relay: true}
	for name, fp := range map[string]func(Config) string{
		"ConfigFingerprint":             ConfigFingerprint,
		"CoreFingerprint":               CoreFingerprint,
		"LiveFingerprint":               LiveFingerprint,
		"PrePolicyCoreFingerprint":      PrePolicyCoreFingerprint,
		"PrePolicyProvisionFingerprint": PrePolicyProvisionFingerprint,
		"PrePolicyLaunchFingerprint":    PrePolicyLaunchFingerprint,
	} {
		if fp(base) != fp(armed) {
			t.Errorf("%s moved when ProgramStatus was armed", name)
		}
	}
}
