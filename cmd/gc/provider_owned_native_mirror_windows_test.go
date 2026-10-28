//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"

	bdpack "github.com/gastownhall/gascity/examples/bd"
)

// nativeBdMirroredScript pins, by SHA-256, each part of the shipped
// gc-beads-bd.sh that provider_owned_native_windows.go mirrors: the functions
// its provider-owned arm runs, and the dispatch that routes to them. A
// function is its text from its "name() {" line through its closing "}".
var nativeBdMirroredScript = map[string]string{
	"die":                                         "7547203de779181a99bf7db4ba832e39a511a5e64ec0a2440a9299c9eda011d1",
	"trim_space":                                  "8f009f68472b45dff4fdf17507bc6930f0ec76f809c10d5a1291f3f285f18682",
	"normalize_dolt_mode":                         "171204388c24f28a13b016093170217ec4cd23a688770ed5bb995a00a8a74a0b",
	"trace_bd_argv":                               "b02209acba940544959a308f0d7077cb91f0f9e822c6e6a5a7d7712313a498bd",
	"read_metadata_string_field":                  "fe90149fc0e7aea108eb5b4f5114feaaaa749948d92f181504fe8d3354b291fd",
	"scope_backend_is_dolt":                       "ab31b3715e0baf781752af679dd98df2074def93a0b16366bfc46e51bea46455",
	"scope_is_proxied":                            "94c04118a08dce319f8a675957afee5cda024ea13eefb5f08c05597eb517c6ed",
	"provider_owned_scope_dir":                    "76284fc9e333827889572032c34bfb72e9793378d326860876c9f0ccbf2edc9b",
	"provider_owned_transport":                    "81d251419656fe3b265ceca4043c4fd4d5b7d170494af90bd5ec2e9584412feb",
	"provider_owned_scope_is_local":               "031b24ee3266a97b68d354d0baf5ef2516bbcb33789483889f60c900ccf624ed",
	"run_provider_owned_bd":                       "13b1a974e3229459db98565f5fa8edce66a66810631d26960ee5e67524e5bea9",
	"op_provider_owned_init":                      "af925b7b186bc0e940bcfb76e250dff059cabc625a37af7ce726ab79bd11cb3d",
	"provider_owned_retire_local_dolt":            "00d6a8c69208ebb278f13c6cb01361e43d12132c57de22248be2b9b082c0b9a8",
	"provider_owned_proxy_root":                   "d30891f44dbdeb42885adb4b672137c26b9fd7d8b05e0099331290e7b07d6f94",
	"provider_owned_scope_shares_city_proxy_root": "56592e07ee0200bead1ea8137aba5157f1d11e09529ab937ef18212522a6ae8b",
	"op_provider_owned_lifecycle":                 "06b4330ff9afcd19cdb19b915d8b28cddb7cc295ea4fcd23e874bd5483a3f27c",
	nativeBdMainDispatch:                          "d49cf18926bff948d997f243da98aaca30f6904fdc7bdb9d6c2e63c071bd4eee",
}

// nativeBdMainDispatch names the script's top-level dispatch: its text from
// "# --- Main ---" up to "# Set derived paths.", which holds the GC_DOLT=skip
// exit, the GC_CITY_PATH check and the provider-owned routing.
const nativeBdMainDispatch = "main dispatch"

// TestNativeBdProviderMirrorsTheScript fails when the shipped gc-beads-bd.sh
// changes a part gc's native provider mirrors. The native form has to follow
// the script: bring provider_owned_native_windows.go into line with the
// change, then update the pin here to the reported sum.
func TestNativeBdProviderMirrorsTheScript(t *testing.T) {
	data, err := bdpack.PackFS.ReadFile("assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatalf("reading the shipped gc-beads-bd.sh: %v", err)
	}
	script := string(data)
	names := make([]string, 0, len(nativeBdMirroredScript))
	for name := range nativeBdMirroredScript {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var part string
		var ok bool
		if name == nativeBdMainDispatch {
			part, ok = scriptSection(script, "# --- Main ---\n", "# Set derived paths.\n")
		} else {
			part, ok = scriptFunction(script, name)
		}
		if !ok {
			t.Errorf("gc-beads-bd.sh no longer has %s, which provider_owned_native_windows.go mirrors", name)
			continue
		}
		sum := sha256.Sum256([]byte(part))
		if got := hex.EncodeToString(sum[:]); got != nativeBdMirroredScript[name] {
			t.Errorf("gc-beads-bd.sh changed %s (sha256 %s): bring provider_owned_native_windows.go into line with it, then update its pin", name, got)
		}
	}
}

// scriptFunction is name's definition in script, from its "name() {" line
// through its closing "}".
func scriptFunction(script, name string) (string, bool) {
	start := strings.Index(script, "\n"+name+"() {\n")
	if start < 0 {
		return "", false
	}
	start++
	end := strings.Index(script[start:], "\n}\n")
	if end < 0 {
		return "", false
	}
	return script[start : start+end+2], true
}

// scriptSection is script's text from begin up to, not including, end.
func scriptSection(script, begin, end string) (string, bool) {
	start := strings.Index(script, begin)
	if start < 0 {
		return "", false
	}
	length := strings.Index(script[start:], end)
	if length < 0 {
		return "", false
	}
	return script[start : start+length], true
}
