package cliauth

import (
	"reflect"
	"regexp"
	"testing"
)

func jsonTags(t reflect.Type) []string {
	tags := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}
		name := tag
		if idx := indexComma(tag); idx >= 0 {
			name = tag[:idx]
		}
		tags = append(tags, name)
	}
	return tags
}

func indexComma(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			return i
		}
	}
	return -1
}

// TestWireFieldsAreStableAndVendorNeutral pins the v0 wire field set so any
// addition is a deliberate, reviewed diff (a commercial field cannot land
// silently), and asserts no field name carries account/commercial semantics —
// such policy travels only in the opaque message/links fields (spec §5).
func TestWireFieldsAreStableAndVendorNeutral(t *testing.T) {
	want := map[string][]string{
		"meResponse":          {"user", "session", "message", "links", "error"},
		"meUser":              {"id", "handle", "display_name"},
		"sessionInfo":         {"created_at", "expires_at", "last_used", "fingerprint"},
		"apiError":            {"code", "message"},
		"deviceCodeResponse":  {"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval", "error"},
		"deviceTokenResponse": {"access_token", "token_type", "error", "interval"},
		"browserLoginResult":  {"token", "service", "state"},
	}
	got := map[string][]string{
		"meResponse":          jsonTags(reflect.TypeOf(meResponse{})),
		"meUser":              jsonTags(reflect.TypeOf(meUser{})),
		"sessionInfo":         jsonTags(reflect.TypeOf(sessionInfo{})),
		"apiError":            jsonTags(reflect.TypeOf(apiError{})),
		"deviceCodeResponse":  jsonTags(reflect.TypeOf(deviceCodeResponse{})),
		"deviceTokenResponse": jsonTags(reflect.TypeOf(deviceTokenResponse{})),
		"browserLoginResult":  jsonTags(reflect.TypeOf(browserLoginResult{})),
	}
	for name, wantTags := range want {
		if !reflect.DeepEqual(got[name], wantTags) {
			t.Fatalf("%s wire fields drifted:\n got  %v\n want %v\n(update the spec + this golden if the change is deliberate)", name, got[name], wantTags)
		}
	}

	commercial := regexp.MustCompile(`(?i)trial|billing|credit|plan|invoice|subscription|quota|coupon|entitlement`)
	for name, tags := range got {
		for _, tag := range tags {
			if commercial.MatchString(tag) {
				t.Fatalf("%s has a commercial wire field %q; such policy must travel only in opaque message/links", name, tag)
			}
		}
	}
}
