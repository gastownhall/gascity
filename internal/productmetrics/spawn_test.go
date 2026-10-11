package productmetrics

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testSpawnTokenOne   = "10000000-0000-4000-8000-000000000001"
	testSpawnTokenTwo   = "20000000-0000-4000-8000-000000000002"
	testSpawnTokenThree = "30000000-0000-4000-8000-000000000003"
)

func TestParsePrivateUploaderInvocationConsumesEverySentinelShape(t *testing.T) {
	t.Parallel()

	valid, detected, err := ParsePrivateUploaderInvocation([]string{privateUploaderSentinel, testSpawnTokenOne})
	if err != nil || !detected || valid.attemptToken != testSpawnTokenOne {
		t.Fatalf("valid private invocation = (%#v, %v, %v)", valid, detected, err)
	}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing token", args: []string{privateUploaderSentinel}},
		{name: "extra argument", args: []string{privateUploaderSentinel, testSpawnTokenOne, "extra"}},
		{name: "malformed token", args: []string{privateUploaderSentinel, "not-a-token"}},
		{name: "uppercase token", args: []string{privateUploaderSentinel, "ABCDEFAB-0000-4000-8000-000000000001"}},
		{name: "wrong UUID version", args: []string{privateUploaderSentinel, "10000000-0000-5000-8000-000000000001"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocation, detected, err := ParsePrivateUploaderInvocation(test.args)
			if !detected || err == nil || invocation != (PrivateUploaderInvocation{}) {
				t.Fatalf("malformed private invocation = (%#v, %v, %v), want consumed error", invocation, detected, err)
			}
		})
	}

	for _, args := range [][]string{nil, {}, {"help"}, {"--version", privateUploaderSentinel}} {
		invocation, detected, err := ParsePrivateUploaderInvocation(args)
		if detected || err != nil || invocation != (PrivateUploaderInvocation{}) {
			t.Fatalf("ordinary args %q = (%#v, %v, %v), want unhandled", args, invocation, detected, err)
		}
	}
}

func TestSpawnThrottleCodecIsCanonicalBoundedAndSchemaClosed(t *testing.T) {
	t.Parallel()

	attempted := time.Date(2026, time.July, 12, 1, 2, 3, 456789000, time.UTC)
	record := spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: attempted}
	encoded, err := encodeSpawnThrottle(record)
	if err != nil {
		t.Fatal(err)
	}
	const want = "throttle_schema = 1\nattempt_token = \"10000000-0000-4000-8000-000000000001\"\nattempted_at = \"2026-07-12T01:02:03.456789Z\"\n"
	if string(encoded) != want {
		t.Fatalf("encoded throttle = %q, want %q", encoded, want)
	}
	if len(encoded) > maximumSpawnThrottleBytes {
		t.Fatalf("encoded throttle is %d bytes, maximum %d", len(encoded), maximumSpawnThrottleBytes)
	}
	decoded, err := decodeSpawnThrottle(encoded)
	if err != nil || decoded != record {
		t.Fatalf("decoded throttle = %#v, %v; want %#v", decoded, err, record)
	}

	for name, body := range map[string]string{
		"empty":           "",
		"unknown field":   want + "extra = true\n",
		"missing field":   strings.Replace(want, "throttle_schema = 1\n", "", 1),
		"future schema":   strings.Replace(want, "throttle_schema = 1", "throttle_schema = 2", 1),
		"duplicate":       want + "attempt_token = \"20000000-0000-4000-8000-000000000002\"\n",
		"noncanonical ID": strings.Replace(want, testSpawnTokenOne, "ABCDEFAB-0000-4000-8000-000000000001", 1),
		"non-v4 ID":       strings.Replace(want, testSpawnTokenOne, "10000000-0000-5000-8000-000000000001", 1),
		"offset instant":  strings.Replace(want, "2026-07-12T01:02:03.456789Z", "2026-07-12T02:02:03.456789+01:00", 1),
		"padded instant":  strings.Replace(want, "2026-07-12T01:02:03.456789Z", "2026-07-12T01:02:03.456789000Z", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSpawnThrottle([]byte(body)); err == nil {
				t.Fatalf("decodeSpawnThrottle(%q) succeeded", body)
			}
		})
	}
	if _, err := decodeSpawnThrottle(make([]byte, maximumSpawnThrottleBytes+1)); err == nil {
		t.Fatal("oversized throttle record decoded")
	}
}

func TestPrivateUploaderEnvironmentIsMinimalDeterministicAndPinsHome(t *testing.T) {
	t.Parallel()

	parent := []string{
		"PATH=/secret/bin", "HOME=/home/alice", "GC_HOME=/wrong", "LANG=en_US.UTF-8", "LC_ALL=C",
		"TMPDIR=/private/tmp/alice", "XDG_CONFIG_HOME=/home/alice/.config", "XDG_CACHE_HOME=/home/alice/.cache",
		"HTTPS_PROXY=http://proxy.example", "NO_PROXY=*", "SSL_CERT_FILE=/secret/ca.pem", "REQUESTS_CA_BUNDLE=/secret/ca.pem",
		"OTEL_EXPORTER_OTLP_HEADERS=secret", "GC_OTEL_ENDPOINT=https://otel", "BD_OTEL_ENDPOINT=https://beads",
		"GC_DISABLE_USAGE_METRICS=1", "DO_NOT_TRACK=1", "GC_COST_MODEL=secret", "API_TOKEN=secret",
		"LANG=fr_FR.UTF-8", "LC_SECRET=must-not-leak", "LC_TIME=en_GB.UTF-8", "GODEBUG=http2debug=1",
	}
	got, err := buildPrivateUploaderEnvironment(parent, "/home/alice/.gc")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GC_HOME=/home/alice/.gc",
		"GC_PRODUCT_METRICS_PRIVATE_UPLOADER=1",
		"HOME=/home/alice",
		"LANG=fr_FR.UTF-8",
		"LC_ALL=C",
		"LC_TIME=en_GB.UTF-8",
		"TMPDIR=/private/tmp/alice",
		"XDG_CACHE_HOME=/home/alice/.cache",
		"XDG_CONFIG_HOME=/home/alice/.config",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("private environment:\n got: %#v\nwant: %#v", got, want)
	}
	for _, entry := range got {
		for _, forbidden := range []string{"PROXY", "CERT", "CA_BUNDLE", "OTEL", "BD_", "USAGE", "COST", "TOKEN", "PATH=", "LC_SECRET", "GODEBUG"} {
			if strings.Contains(entry, forbidden) {
				t.Fatalf("private environment leaked forbidden class %q in %q", forbidden, entry)
			}
		}
	}
}

func TestPrivateUploaderEnvironmentBoundsAndNormalizesAllowedValues(t *testing.T) {
	t.Parallel()
	tooLong := strings.Repeat("a", 65)
	got, err := buildPrivateUploaderEnvironment([]string{
		"HOME=relative", "TMPDIR=/private/tmp/alice/", "XDG_STATE_HOME=/home/alice/state/../state",
		"LANG=  en_US.UTF-8  ", "LC_TIME=" + tooLong, "LC_SECRET=C", "LD_PRELOAD=/tmp/inject.so",
	}, "/home/alice/.gc/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GC_HOME=/home/alice/.gc",
		"GC_PRODUCT_METRICS_PRIVATE_UPLOADER=1",
		"LANG=en_US.UTF-8",
		"TMPDIR=/private/tmp/alice",
		"XDG_STATE_HOME=/home/alice/state",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized private environment = %#v, want %#v", got, want)
	}
}
