package main

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestClassifyProductMetricsCommandCanonicalMatrix(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	for _, test := range []struct {
		name      string
		args      []string
		wantID    productMetricsCommandID
		recording productMetricsRecordingPolicy
	}{
		{name: "bare root", args: nil, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "deferred help group", args: []string{"analyze"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "deferred unknown group", args: []string{"agent"}, wantID: productMetricsCommandUnknown, recording: productMetricsRecordingRecordable},
		{name: "target help", args: []string{"session", "peek", "--help"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "help after positional", args: []string{"session", "peek", "private-value", "--help"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "help after terminator is data", args: []string{"session", "peek", "--", "--help"}, wantID: productMetricsGeneratedCommandID147, recording: productMetricsRecordingRecordable},
		{name: "recognized invalid flag", args: []string{"session", "peek", "--not-a-flag"}, wantID: productMetricsGeneratedCommandID147, recording: productMetricsRecordingRecordable},
		{name: "recognized invalid arg", args: []string{"session", "peek", "one", "two"}, wantID: productMetricsGeneratedCommandID147, recording: productMetricsRecordingRecordable},
		{name: "canonical alias", args: []string{"cities", "ls"}, wantID: productMetricsGeneratedCommandID19, recording: productMetricsRecordingRecordable},
		{name: "child long flag before command", args: []string{"--target", "remote", "handoff", "subject"}, wantID: productMetricsGeneratedCommandID61, recording: productMetricsRecordingRecordable},
		{name: "child long flag value matches command", args: []string{"--target", "status", "handoff", "subject"}, wantID: productMetricsGeneratedCommandID61, recording: productMetricsRecordingRecordable},
		{name: "child shorthand before command path", args: []string{"-f", "/tmp/config.toml", "config", "show"}, wantID: productMetricsGeneratedCommandID22, recording: productMetricsRecordingRecordable},
		{name: "child shorthand between command words", args: []string{"config", "-f", "/tmp/config.toml", "show"}, wantID: productMetricsGeneratedCommandID22, recording: productMetricsRecordingRecordable},
		{name: "unknown split long consumes command-looking value", args: []string{"--bogus", "status"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "unknown equal long leaves command word", args: []string{"--bogus=status", "status"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "unknown split short consumes command-looking value", args: []string{"-x", "status"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "unknown equal short leaves command word", args: []string{"-x=status", "status"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "unknown root", args: []string{"not-a-command"}, wantID: productMetricsCommandUnknown, recording: productMetricsRecordingRecordable},
		{name: "root help wins unknown", args: []string{"not-a-command", "--help"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "root help consumes completion during find", args: []string{"--help", "completion", "bash"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "root help consumes metrics and finds status", args: []string{"--help", "metrics", "status"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "metrics help consumes nested status", args: []string{"metrics", "--help", "status"}, recording: productMetricsRecordingExcluded},
		{name: "completion group owns help", args: []string{"completion", "--help", "bash"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "unknown nested structural", args: []string{"completion", "bogus"}, wantID: productMetricsCommandUnknown, recording: productMetricsRecordingRecordable},
		{name: "nested help wins unknown", args: []string{"completion", "bogus", "--help"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "help false retains command", args: []string{"start", "--help=false"}, wantID: productMetricsGeneratedCommandID162, recording: productMetricsRecordingRecordable},
		{name: "help zero retains command", args: []string{"start", "--help=0"}, wantID: productMetricsGeneratedCommandID162, recording: productMetricsRecordingRecordable},
		{name: "help true form", args: []string{"start", "--help=TRUE"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "short help bare", args: []string{"start", "-h"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "short help lower true", args: []string{"start", "-h=t"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "short help upper short true", args: []string{"start", "-h=T"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "short help upper true", args: []string{"start", "-h=TRUE"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "short help one", args: []string{"start", "-h=1"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "short bool cluster includes help", args: []string{"start", "-nh"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "attached shorthand value before help", args: []string{"config", "show", "-f/tmp/nope", "--help"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "short help false retains command", args: []string{"start", "-h=false"}, wantID: productMetricsGeneratedCommandID162, recording: productMetricsRecordingRecordable},
		{name: "short help consumed as flag value", args: []string{"handoff", "--target", "-h"}, wantID: productMetricsGeneratedCommandID61, recording: productMetricsRecordingRecordable},
		{name: "bd long help is passthrough data", args: []string{"bd", "--help"}, wantID: productMetricsGeneratedCommandID11, recording: productMetricsRecordingRecordable},
		{name: "bd valued help is passthrough data", args: []string{"bd", "--help=true"}, wantID: productMetricsGeneratedCommandID11, recording: productMetricsRecordingRecordable},
		{name: "bd short help is passthrough data", args: []string{"bd", "-h"}, wantID: productMetricsGeneratedCommandID11, recording: productMetricsRecordingRecordable},
		{name: "bd terminated help is passthrough data", args: []string{"bd", "--", "--help"}, wantID: productMetricsGeneratedCommandID11, recording: productMetricsRecordingRecordable},
		{name: "beads list long help is manual data", args: []string{"beads", "list", "--help"}, wantID: productMetricsGeneratedCommandID15, recording: productMetricsRecordingRecordable},
		{name: "beads list valued help is manual data", args: []string{"beads", "list", "--help=true"}, wantID: productMetricsGeneratedCommandID15, recording: productMetricsRecordingRecordable},
		{name: "beads list terminated help is manual data", args: []string{"beads", "list", "--", "--help"}, wantID: productMetricsGeneratedCommandID15, recording: productMetricsRecordingRecordable},
		{name: "root help before manual leaf", args: []string{"--help", "beads", "list"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "beads group help before manual leaf", args: []string{"beads", "--help", "list"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "beads list valued short help is manual data", args: []string{"beads", "list", "-h=t"}, wantID: productMetricsGeneratedCommandID15, recording: productMetricsRecordingRecordable},
		{name: "split schema role before command", args: []string{"--json-schema", "result", "status"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "split schema role after command", args: []string{"status", "--json-schema", "failure"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "equal schema role before command", args: []string{"--json-schema=result", "status"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "equal schema role after command", args: []string{"status", "--json-schema=manifest"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "bare schema before command", args: []string{"--json-schema", "status"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "schema role without command", args: []string{"--json-schema", "result"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "schema request beats help", args: []string{"status", "--json-schema", "--help"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "supported json falls through to help", args: []string{"status", "--json", "--help"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "unsupported json beats help", args: []string{"completion", "bash", "--json", "--help"}, wantID: productMetricsGeneratedCommandID20, recording: productMetricsRecordingRecordable},
		{name: "unknown json beats help", args: []string{"not-a-command", "--json", "--help"}, wantID: productMetricsCommandUnknown, recording: productMetricsRecordingRecordable},
		{name: "unrecognized json spelling keeps root flag failure", args: []string{"not-a-command", "--json=TRUE", "--help"}, wantID: productMetricsCommandHelp, recording: productMetricsRecordingRecordable},
		{name: "unknown flag before help retains completion", args: []string{"completion", "bash", "--bogus", "--help"}, wantID: productMetricsGeneratedCommandID20, recording: productMetricsRecordingRecordable},
		{name: "unknown flag after help retains completion", args: []string{"completion", "bash", "--help", "--bogus"}, wantID: productMetricsGeneratedCommandID20, recording: productMetricsRecordingRecordable},
		{name: "unknown flag before help retains status", args: []string{"status", "--bogus", "--help"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "unrecognized json spelling beats help parse", args: []string{"completion", "bash", "--json=TRUE", "--help"}, wantID: productMetricsGeneratedCommandID20, recording: productMetricsRecordingRecordable},
		{name: "invalid known bool beats help", args: []string{"status", "--json=bogus", "--help"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "invalid duration beats help", args: []string{"stop", "--timeout", "bogus", "--help"}, wantID: productMetricsGeneratedCommandID164, recording: productMetricsRecordingRecordable},
		{name: "user completion", args: []string{"completion", "bash"}, wantID: productMetricsGeneratedCommandID20, recording: productMetricsRecordingRecordable},
		{name: "private completion", args: []string{"__complete", "status"}, recording: productMetricsRecordingExcluded},
		{name: "private completion alias", args: []string{"__completeNoDesc", "status"}, recording: productMetricsRecordingExcluded},
		{name: "private completion after split root scope", args: []string{"--city", "/tmp/city", "__complete", "status"}, recording: productMetricsRecordingExcluded},
		{name: "private completion alias after equal root scope", args: []string{"--city=/tmp/city", "__completeNoDesc", "status"}, recording: productMetricsRecordingExcluded},
		{name: "private completion sentinel consumed as scope value", args: []string{"--city", "__complete", "status"}, wantID: productMetricsGeneratedCommandID163, recording: productMetricsRecordingRecordable},
		{name: "private completion after terminator is data", args: []string{"--", "__complete", "status"}, wantID: productMetricsCommandUnknown, recording: productMetricsRecordingRecordable},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := classifyProductMetricsCommand(root, test.args, productMetricsPolicyContext{})
			if got.ID != test.wantID || got.Recording != test.recording {
				t.Fatalf("classification = %+v, want id=%d recording=%q", got, test.wantID, test.recording)
			}
		})
	}
}

func TestClassifyProductMetricsCommandPolicyMatrix(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	for _, test := range []struct {
		name      string
		args      []string
		context   productMetricsPolicyContext
		notice    productMetricsNoticePolicy
		recording productMetricsRecordingPolicy
		reason    productMetricsExclusionReason
	}{
		{name: "ordinary", args: []string{"start"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "root help before completion is eligible", args: []string{"--help", "completion", "bash"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "completion group help is ineligible", args: []string{"completion", "--help", "bash"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "generic json", args: []string{"start", "--json"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "generic jsonl format", args: []string{"status", "--format=jsonl"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "split machine flag before command", args: []string{"--format", "json", "status"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "generic format last text", args: []string{"status", "--format", "json", "--format", "text"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "generic format last json", args: []string{"status", "--format", "text", "--format", "json"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "generic json last false", args: []string{"status", "--json", "--json=false"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "generic json last true", args: []string{"status", "--json=false", "--json"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "generic pflag uppercase json", args: []string{"status", "--json=TRUE"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "generic pflag uppercase json last false", args: []string{"status", "--json=TRUE", "--json=false"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "generic pflag uppercase json last true", args: []string{"status", "--json=false", "--json=TRUE"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads toon", args: []string{"beads", "list", "--format", "toon"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads toon after terminator", args: []string{"beads", "list", "--", "--format", "toon"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads json after terminator", args: []string{"beads", "list", "--", "--json"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads format last text", args: []string{"beads", "list", "--format", "json", "--format", "text"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "beads format last toon", args: []string{"beads", "list", "--format", "text", "--format", "toon"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads early json then text", args: []string{"beads", "list", "--json", "--format", "text"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads text then json", args: []string{"beads", "list", "--format", "text", "--json"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads early json before terminator", args: []string{"beads", "list", "--json", "--", "--format", "text"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads manual json then text after terminator", args: []string{"beads", "list", "--", "--json", "--format", "text"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "beads manual text then json after terminator", args: []string{"beads", "list", "--", "--format", "text", "--json"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "beads disabled early json then text", args: []string{"beads", "list", "--json", "--json=false", "--format", "text"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "beads disabled early json retains manual json", args: []string{"beads", "list", "--json", "--json=false"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
		{name: "prime hook", args: []string{"prime", "--hook"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionPrimeHook},
		{name: "prime hook true form", args: []string{"prime", "--hook=TRUE"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionPrimeHook},
		{name: "prime hook false", args: []string{"prime", "--hook=false"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "prime hook last false", args: []string{"prime", "--hook", "--hook=false"}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "prime hook last true", args: []string{"prime", "--hook=false", "--hook"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionPrimeHook},
		{name: "prime hook format last empty", args: []string{"prime", "--hook-format=x", "--hook-format="}, notice: productMetricsNoticeEligible, recording: productMetricsRecordingRecordable},
		{name: "prime hook format last nonempty", args: []string{"prime", "--hook-format=", "--hook-format=x"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionPrimeHook},
		{name: "split hook flag before command", args: []string{"--hook-format", "claude", "handoff", "subject"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionHandoffAutomation},
		{name: "split hook flag between command words", args: []string{"mail", "--hook-format", "claude", "check"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionMailHookFormat},
		{name: "handoff auto", args: []string{"handoff", "--auto"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionHandoffAutomation},
		{name: "mail inject", args: []string{"mail", "check", "--inject"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionMailHookFormat},
		{name: "managed", args: []string{"start"}, context: productMetricsPolicyContext{ManagedAutomation: true}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionManagedContext},
		{name: "provider hook", args: []string{"start"}, context: productMetricsPolicyContext{ProviderHook: true}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionProviderHook},
		{name: "managed wins provider", args: []string{"start"}, context: productMetricsPolicyContext{ManagedAutomation: true, ProviderHook: true}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionManagedContext},
		{name: "static exclusion wins", args: []string{"metrics", "status", "--json"}, context: productMetricsPolicyContext{ManagedAutomation: true}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingExcluded, reason: productMetricsExclusionMetricsControl},
		{name: "raw json pre-scan wins flag value", args: []string{"handoff", "--target", "--json"}, notice: productMetricsNoticeIneligible, recording: productMetricsRecordingRecordable},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := classifyProductMetricsCommand(root, test.args, test.context)
			if got.Notice != test.notice || got.Recording != test.recording || got.Exclusion != test.reason {
				t.Fatalf("classification = %+v, want notice=%q recording=%q reason=%q", got, test.notice, test.recording, test.reason)
			}
		})
	}
}

func TestClassifyProductMetricsBeadsEarlyJSONMatchesControlOutput(t *testing.T) {
	const wantOutput = "{\"schema_version\":\"1\",\"ok\":false,\"error\":{\"code\":\"json_unsupported\",\"message\":\"command \\\"beads list\\\" does not declare JSON support\",\"exit_code\":1}}\n"
	for _, args := range [][]string{
		{"beads", "list", "--json", "--format", "text"},
		{"beads", "list", "--format", "text", "--json"},
	} {
		t.Run(strings.Join(args[2:], "_"), func(t *testing.T) {
			configureIsolatedRuntimeEnv(t)
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 1 {
				t.Fatalf("run = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stdout.String() != wantOutput || stderr.Len() != 0 {
				t.Fatalf("control output = stdout %q stderr %q, want stdout %q and empty stderr", stdout.String(), stderr.String(), wantOutput)
			}

			root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
			got := classifyProductMetricsCommand(root, args, productMetricsPolicyContext{})
			if got.Notice != productMetricsNoticeIneligible || got.Recording != productMetricsRecordingRecordable {
				t.Fatalf("classification = %+v, want notice-ineligible recordable", got)
			}
		})
	}
}

func TestClassifyProductMetricsPackWildcardCannotReturnDynamicName(t *testing.T) {
	const secret = "private-pack-command-7f25"
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	dynamic := &cobra.Command{Use: secret, Annotations: map[string]string{productMetricsClassAnnotation: packCommandClassificationValue}, Run: func(*cobra.Command, []string) {}}
	root.AddCommand(dynamic)
	got := classifyProductMetricsCommand(root, []string{secret, "private-argument"}, productMetricsPolicyContext{})
	if got.ID != productMetricsCommandPackCommand || got.Recording != productMetricsRecordingRecordable || got.Notice != productMetricsNoticeIneligible {
		t.Fatalf("pack classification = %+v", got)
	}
	formatted := fmt.Sprintf("%+v", got)
	for _, privateValue := range []string{secret, "private-argument"} {
		if strings.Contains(formatted, privateValue) {
			t.Fatalf("classification leaked private value %q", privateValue)
		}
	}
	resultType := reflect.TypeOf(got)
	for index := 0; index < resultType.NumField(); index++ {
		field := resultType.Field(index)
		if strings.Contains(strings.ToLower(field.Name), "arg") || strings.Contains(strings.ToLower(field.Name), "path") || field.Type == reflect.TypeOf((*cobra.Command)(nil)) || field.Type.Kind() == reflect.Slice {
			t.Fatalf("classification has privacy-unsafe field %s %s", field.Name, field.Type)
		}
	}
}

func TestClassifyProductMetricsPackHonorsContextExclusions(t *testing.T) {
	for name, context := range map[string]productMetricsPolicyContext{
		"managed":  {ManagedAutomation: true},
		"provider": {ProviderHook: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := classifyProductMetricsPackOutcome(packCommandOutcome{handled: true, classification: packCommandClassification}, context)
			if got.Recording != productMetricsRecordingExcluded || got.ID != 0 {
				t.Fatalf("pack outcome = %+v", got)
			}
		})
	}
}

func TestClassifyProductMetricsEagerPackHonorsContextExclusions(t *testing.T) {
	for _, test := range []struct {
		name    string
		context productMetricsPolicyContext
		reason  productMetricsExclusionReason
	}{
		{name: "managed", context: productMetricsPolicyContext{ManagedAutomation: true}, reason: productMetricsExclusionManagedContext},
		{name: "provider", context: productMetricsPolicyContext{ProviderHook: true}, reason: productMetricsExclusionProviderHook},
		{name: "managed precedence", context: productMetricsPolicyContext{ManagedAutomation: true, ProviderHook: true}, reason: productMetricsExclusionManagedContext},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
			root.AddCommand(&cobra.Command{
				Use:         "dynamic-pack",
				Annotations: map[string]string{productMetricsClassAnnotation: packCommandClassificationValue},
				Run:         func(*cobra.Command, []string) {},
			})
			got := classifyProductMetricsCommand(root, []string{"dynamic-pack"}, test.context)
			if got.ID != 0 || got.Recording != productMetricsRecordingExcluded || got.Exclusion != test.reason {
				t.Fatalf("eager pack classification = %+v, want excluded reason %q", got, test.reason)
			}
		})
	}
}

func TestClassifyProductMetricsCommandRejectsAnnotationDrift(t *testing.T) {
	for name, mutate := range map[string]func(*cobra.Command){
		"known id swap": func(command *cobra.Command) {
			command.Annotations[productMetricsIDAnnotation] = fmt.Sprint(productMetricsGeneratedCommandID19)
		},
		"registered mode swap": func(command *cobra.Command) {
			command.Annotations[productMetricsModeAnnotation] = string(productMetricsModeVersion)
		},
		"conditional deleted": func(command *cobra.Command) { delete(command.Annotations, productMetricsConditionalAnnotation) },
		"built-in class changed to pack wildcard": func(command *cobra.Command) {
			command.Annotations[productMetricsClassAnnotation] = packCommandClassificationValue
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
			path := "gc start"
			args := []string{"start"}
			if name == "conditional deleted" {
				path, args = "gc prime", []string{"prime"}
			}
			command, ok := findCommandByCanonicalPath(root, path)
			if !ok {
				t.Fatal("missing command")
			}
			mutate(command)
			got := classifyProductMetricsCommand(root, args, productMetricsPolicyContext{})
			if got.Recording != productMetricsRecordingExcluded || got.Exclusion != productMetricsExclusionCensusMismatch {
				t.Fatalf("annotation drift classification = %+v", got)
			}
		})
	}
}

func TestClassifyProductMetricsPackFailsClosedWhenCensusIsStale(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "secret-pack", Annotations: map[string]string{productMetricsClassAnnotation: packCommandClassificationValue}, Run: func(*cobra.Command, []string) {}})
	got := classifyProductMetricsCommand(root, []string{"secret-pack"}, productMetricsPolicyContext{})
	if got.Recording != productMetricsRecordingExcluded || got.Exclusion != productMetricsExclusionCensusMismatch || got.ID != 0 {
		t.Fatalf("stale census classification = %+v", got)
	}
}

func TestClassifyProductMetricsCommandDoesNotMutateLiveFlags(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	for _, test := range []struct {
		path string
		flag string
		args []string
	}{
		{path: "gc status", flag: "json", args: []string{"status", "--json=TRUE"}},
		{path: "gc stop", flag: "timeout", args: []string{"stop", "--timeout", "3s", "--help"}},
		{path: "gc config show", flag: "file", args: []string{"config", "show", "-f/tmp/config.toml", "--help"}},
	} {
		command, ok := findCommandByCanonicalPath(root, test.path)
		if !ok {
			t.Fatalf("missing command %q", test.path)
		}
		flag := lookupCommandFlag(command, test.flag)
		if flag == nil {
			t.Fatalf("missing flag %q on %q", test.flag, test.path)
		}
		beforeValue, beforeChanged := flag.Value.String(), flag.Changed
		_ = classifyProductMetricsCommand(root, test.args, productMetricsPolicyContext{})
		if flag.Value.String() != beforeValue || flag.Changed != beforeChanged {
			t.Fatalf("classification mutated %s --%s: value=%q changed=%t, want value=%q changed=%t", test.path, test.flag, flag.Value.String(), flag.Changed, beforeValue, beforeChanged)
		}
	}
}

func TestProductMetricsRegistriesRejectDuplicateAndMissingCallbacks(t *testing.T) {
	originalStatic := productMetricsStaticModeRegistry
	originalConditional := productMetricsConditionalRegistry
	originalResolvers := productMetricsResolverRegistry
	t.Cleanup(func() {
		productMetricsStaticModeRegistry = originalStatic
		productMetricsConditionalRegistry = originalConditional
		productMetricsResolverRegistry = originalResolvers
	})

	productMetricsStaticModeRegistry = append(append([]productMetricsStaticModeRegistration(nil), originalStatic...), originalStatic[0])
	if err := validateDeferredProductMetricsResolvers(generatedProductMetricsCommandCensus, generatedProductMetricsSyntheticCensus); err == nil {
		t.Fatal("duplicate static mode was accepted")
	}
	productMetricsStaticModeRegistry = originalStatic
	productMetricsConditionalRegistry = append([]productMetricsConditionalRegistration(nil), originalConditional...)
	productMetricsConditionalRegistry[0].Apply = nil
	if err := validateDeferredProductMetricsResolvers(generatedProductMetricsCommandCensus, generatedProductMetricsSyntheticCensus); err == nil {
		t.Fatal("nil conditional callback was accepted")
	}
	productMetricsConditionalRegistry = originalConditional
	productMetricsResolverRegistry = originalResolvers[:len(originalResolvers)-1]
	if err := validateDeferredProductMetricsResolvers(generatedProductMetricsCommandCensus, generatedProductMetricsSyntheticCensus); err == nil {
		t.Fatal("missing pack resolver was accepted")
	}
}
