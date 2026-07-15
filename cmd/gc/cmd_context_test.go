package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/clientcontext"
)

func newTestContext(name, url, city string) clientcontext.Context {
	return clientcontext.Context{Name: name, URL: url, City: city}
}

func TestDoContextAddThenList(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := doContextAdd(newTestContext("prod", "https://box:9443", "example-city"), &out, &errb); code != 0 {
		t.Fatalf("add code=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	if code := doContextList(false, &out, &errb); code != 0 {
		t.Fatalf("list code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "prod") || !strings.Contains(out.String(), "example-city") {
		t.Errorf("list missing context: %q", out.String())
	}
}

func TestDoContextAddRejectsDuplicate(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	c := newTestContext("prod", "https://box:9443", "")
	if code := doContextAdd(c, &out, &errb); code != 0 {
		t.Fatalf("first add failed: %q", errb.String())
	}
	errb.Reset()
	if code := doContextAdd(c, &out, &errb); code == 0 {
		t.Fatalf("duplicate add should fail")
	}
	if !strings.Contains(errb.String(), "already exists") {
		t.Errorf("want already-exists error, got %q", errb.String())
	}
}

func TestDoContextAddRejectsInvalidURL(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := doContextAdd(newTestContext("bad", "http://evil.example.com", ""), &out, &errb); code == 0 {
		t.Fatalf("non-loopback http should be rejected")
	}
}

func TestDoContextUseSetsAndStars(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	_ = doContextAdd(newTestContext("prod", "https://box:9443", ""), &out, &errb)
	if code := doContextUse("prod", &out, &errb); code != 0 {
		t.Fatalf("use code=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	_ = doContextList(false, &out, &errb)
	line := firstLineContaining(out.String(), "prod")
	if !strings.HasPrefix(strings.TrimSpace(line), "*") {
		t.Errorf("default context should be starred, got %q", line)
	}
}

func TestDoContextUseRejectsUndefined(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := doContextUse("ghost", &out, &errb); code == 0 {
		t.Fatalf("use of undefined context should fail")
	}
}

func TestDoContextRemoveClearsDefault(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	_ = doContextAdd(newTestContext("prod", "https://box:9443", ""), &out, &errb)
	_ = doContextUse("prod", &out, &errb)
	out.Reset()
	if code := doContextRemove("prod", &out, &errb); code != 0 {
		t.Fatalf("remove code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "no default context") {
		t.Errorf("remove of default should note cleared default, got %q", out.String())
	}
	// Reloading must show no contexts and no dangling default.
	file, err := clientcontext.Load(DefaultPath())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(file.Contexts) != 0 || file.Default != "" {
		t.Errorf("after remove: contexts=%d default=%q", len(file.Contexts), file.Default)
	}
}

func TestDoContextListJSON(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	_ = doContextAdd(clientcontext.Context{Name: "prod", URL: "https://box:9443", City: "mc", GrantCommand: "mint"}, &out, &errb)
	_ = doContextUse("prod", &out, &errb)
	out.Reset()
	if code := doContextList(true, &out, &errb); code != 0 {
		t.Fatalf("list json code=%d", code)
	}
	var rec contextJSON
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &rec); err != nil {
		t.Fatalf("json decode: %v (%q)", err, out.String())
	}
	if rec.Name != "prod" || rec.City != "mc" || !rec.Default || rec.GrantCommand != "mint" {
		t.Errorf("json rec = %+v", rec)
	}
}

func TestDoContextCurrentRemoteViaContextFlag(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	_ = doContextAdd(clientcontext.Context{Name: "prod", URL: "https://box:9443", City: "mc", GrantCommand: "mint"}, &out, &errb)

	prev := contextFlag
	contextFlag = "prod"
	defer func() { contextFlag = prev }()

	out.Reset()
	if code := doContextCurrent(&out, &errb); code != 0 {
		t.Fatalf("current code=%d stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "target: mc @ https://box:9443") || !strings.Contains(s, remoteSourceContextFlag) {
		t.Errorf("current output = %q", s)
	}
	if !strings.Contains(s, "grant:mint") {
		t.Errorf("current should report the credential technique: %q", s)
	}
}

func TestDoContextCurrentConflictErrors(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	_ = doContextAdd(clientcontext.Context{Name: "prod", URL: "https://box:9443", City: "mc"}, &out, &errb)

	prevCtx, prevURL := contextFlag, cityURLFlag
	contextFlag, cityURLFlag = "prod", "https://other:9443"
	defer func() { contextFlag, cityURLFlag = prevCtx, prevURL }()

	errb.Reset()
	if code := doContextCurrent(&out, &errb); code == 0 {
		t.Fatalf("current should surface a remote+remote conflict")
	}
	if !strings.Contains(errb.String(), "conflicting") {
		t.Errorf("want conflict error, got %q", errb.String())
	}
}

func firstLineContaining(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}
