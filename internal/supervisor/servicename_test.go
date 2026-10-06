package supervisor

import "testing"

// The suffix is part of every installed platform unit's name
// (gascity-supervisor-<suffix>.service, com.gascity.supervisor.<suffix>), so
// changing it orphans units already on operators' machines. These goldens pin
// the exact names.
func TestServiceSuffix(t *testing.T) {
	for _, tc := range []struct {
		gcHome string
		want   string
	}{
		{"", ""},
		{"/tmp/gc-int-env-123/gc-home", "gc-home-76f7c603"},
		{"/srv/My Gas_City.Home", "my-gas-city-home-171337c9"},
		{"/", "isolated-42099b4a"},
	} {
		if got := ServiceSuffix(tc.gcHome); got != tc.want {
			t.Errorf("ServiceSuffix(%q) = %q, want %q", tc.gcHome, got, tc.want)
		}
	}
}

func TestSanitizeServiceName(t *testing.T) {
	for in, want := range map[string]string{
		"gc-home":           "gc-home",
		"My Gas_City.Home":  "my-gas-city-home",
		"--Leading/Trail--": "leading-trail",
		"...":               "",
	} {
		if got := SanitizeServiceName(in); got != want {
			t.Errorf("SanitizeServiceName(%q) = %q, want %q", in, got, want)
		}
	}
}
