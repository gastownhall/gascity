package beads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	bdhttp "github.com/steveyegge/beads/backend/http"
)

// shQuote single-quotes s for sh -c.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestAmbientCredentialRunsTheTokenCommandOutsideTheEnvMutex: the ambient
// ladder's token command is a subprocess bd allows 30s, and the env mutex
// fences every native open and bd-child env build in the process, so the
// command must never run under it.
func TestAmbientCredentialRunsTheTokenCommandOutsideTheEnvMutex(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	// The token command blocks reading a FIFO whose write end the test holds.
	// Opening that end blocks until the command opens the read end, so the
	// snapshot below is ordered after the command is running by the command's
	// own lifecycle rather than by elapsed wall time; closing it lets the
	// command print its token. read and echo are shell builtins.
	release := filepath.Join(t.TempDir(), "release")
	if err := syscall.Mkfifo(release, 0o600); err != nil {
		t.Fatalf("Mkfifo(%q) = %v", release, err)
	}
	t.Setenv(bdhttp.TokenCommandEnv, "127.0.0.1=read _ < "+shQuote(release)+"; echo blocked-token")
	base, err := url.Parse("http://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}

	type writeEnd struct {
		f   *os.File
		err error
	}
	opened := make(chan writeEnd, 1)
	go func() {
		f, err := os.OpenFile(release, os.O_WRONLY, 0)
		opened <- writeEnd{f, err}
	}()
	t.Cleanup(func() {
		// A command that never ran leaves the open above blocked; a
		// non-blocking read open releases it.
		if f, err := os.OpenFile(release, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	resolved := make(chan *resolvedAmbientCredential, 1)
	go func() { resolved <- newResolvedAmbientCredential(context.Background(), base) }()
	var held writeEnd
	select {
	case held = <-opened:
	case <-time.After(30 * time.Second):
		t.Fatal("the ambient ladder never ran its token command")
	}
	if held.err != nil {
		t.Fatalf("open the token command's FIFO: %v", held.err)
	}
	t.Cleanup(func() { _ = held.f.Close() })

	snapshot := make(chan struct{})
	go func() {
		_ = ProcessEnvSnapshotExcludingNativeDoltOpen()
		close(snapshot)
	}()
	select {
	case <-snapshot:
	case <-time.After(10 * time.Second):
		t.Fatal("an env snapshot waited on the ambient ladder's token command: the command ran under nativeDoltOpenEnvMu")
	}

	if err := held.f.Close(); err != nil {
		t.Fatalf("close the token command's FIFO: %v", err)
	}
	var credential *resolvedAmbientCredential
	select {
	case credential = <-resolved:
	case <-time.After(30 * time.Second):
		t.Fatal("the ladder never resolved after the token command was released")
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9/v0/beads/context", nil)
	if err := credential.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer blocked-token" {
		t.Fatalf("Authorization = %q, want the token command's token", got)
	}
}

// TestSequesteredCredentialEnvReadsUnderTheVaultMutexAlone: an env: source is
// read on every request, so once its name has been looked at, reading it must
// not wait on a Dolt open's env projection. A value exported again still takes
// the guarded path and replaces the vaulted one.
func TestSequesteredCredentialEnvReadsUnderTheVaultMutexAlone(t *testing.T) {
	resetCredentialEnvVault()
	t.Cleanup(resetCredentialEnvVault)
	const vaulted, unset = "GC_TEST_VAULTED_TOKEN", "GC_TEST_UNSET_TOKEN"
	t.Setenv(vaulted, "first-value")
	t.Setenv(unset, "")
	if err := os.Unsetenv(unset); err != nil {
		t.Fatal(err)
	}
	if got := sequesterCredentialEnv(vaulted); got != "first-value" {
		t.Fatalf("first read = %q, want first-value", got)
	}
	if _, ok := os.LookupEnv(vaulted); ok {
		t.Fatalf("%s still in the process environment after its first read", vaulted)
	}
	if got := sequesterCredentialEnv(unset); got != "" {
		t.Fatalf("first read of an unset name = %q, want empty", got)
	}

	// readWhileProjecting reads name while a projection holds the env mutex.
	readWhileProjecting := func(name string) string {
		t.Helper()
		got := make(chan string, 1)
		nativeDoltOpenEnvMu.Lock()
		go func() { got <- sequesterCredentialEnv(name) }()
		select {
		case value := <-got:
			nativeDoltOpenEnvMu.Unlock()
			return value
		case <-time.After(5 * time.Second):
			nativeDoltOpenEnvMu.Unlock()
			<-got
			t.Fatalf("reading %s waited on nativeDoltOpenEnvMu", name)
			return ""
		}
	}
	if got := readWhileProjecting(vaulted); got != "first-value" {
		t.Fatalf("vaulted read = %q, want first-value", got)
	}
	if got := readWhileProjecting(unset); got != "" {
		t.Fatalf("read of an unset name = %q, want empty", got)
	}

	if err := os.Setenv(vaulted, "second-value"); err != nil {
		t.Fatal(err)
	}
	if got := sequesterCredentialEnv(vaulted); got != "second-value" {
		t.Fatalf("read after a re-export = %q, want second-value", got)
	}
	if _, ok := os.LookupEnv(vaulted); ok {
		t.Fatalf("%s still in the process environment after the re-export was read", vaulted)
	}
	if got := readWhileProjecting(vaulted); got != "second-value" {
		t.Fatalf("vaulted read after the re-export = %q, want second-value", got)
	}
}

// scriptedLadder is a bdhttp.CredentialProvider whose walks answer from walk,
// called with the 1-based walk number.
type scriptedLadder struct {
	walk  func(n int) (string, error)
	walks int
	token string
}

func (l *scriptedLadder) Authorize(_ context.Context, req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+l.token)
	return nil
}

func (l *scriptedLadder) Refresh(context.Context) (bool, error) {
	l.walks++
	token, err := l.walk(l.walks)
	if err != nil {
		return false, err
	}
	changed := token != l.token
	l.token = token
	return changed, nil
}

// overlapAProjection runs one env projection, as a concurrent Dolt open would
// in the middle of a walk.
func overlapAProjection(t *testing.T) {
	t.Helper()
	restore, err := withNativeDoltOpenEnv(map[string]string{"BEADS_CREDENTIALS_FILE": filepath.Join(t.TempDir(), "elsewhere")})
	if err != nil {
		t.Fatal(err)
	}
	restore()
}

func authorizeHeader(t *testing.T, c *resolvedAmbientCredential) (string, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9/v0/beads/context", nil)
	err := c.Authorize(context.Background(), req)
	return req.Header.Get("Authorization"), err
}

// TestAmbientCredentialWalksAgainWhenAProjectionOverlappedTheWalk: a walk an
// env projection overlapped may have read the projection rather than the
// operator's environment, so it is walked again; a ladder the projections
// never leave alone stops at ambientCredentialWalks and keeps the last walk.
func TestAmbientCredentialWalksAgainWhenAProjectionOverlappedTheWalk(t *testing.T) {
	for _, tc := range []struct {
		name       string
		overlapped int
		wantWalks  int
		wantHeader string
	}{
		{name: "no overlap", overlapped: 0, wantWalks: 1, wantHeader: "Bearer walk-1"},
		{name: "one overlap", overlapped: 1, wantWalks: 2, wantHeader: "Bearer walk-2"},
		{name: "every walk overlapped", overlapped: ambientCredentialWalks + 1, wantWalks: ambientCredentialWalks, wantHeader: fmt.Sprintf("Bearer walk-%d", ambientCredentialWalks)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ladder := &scriptedLadder{walk: func(n int) (string, error) {
				if n <= tc.overlapped {
					overlapAProjection(t)
				}
				return fmt.Sprintf("walk-%d", n), nil
			}}
			c := &resolvedAmbientCredential{inner: ladder}
			header, err := authorizeHeader(t, c)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if ladder.walks != tc.wantWalks || header != tc.wantHeader {
				t.Fatalf("walks = %d, Authorization = %q; want %d walks and %q", ladder.walks, header, tc.wantWalks, tc.wantHeader)
			}
			if _, err := authorizeHeader(t, c); err != nil || ladder.walks != tc.wantWalks {
				t.Fatalf("a second request walked the ladder again (walks %d, err %v); the resolution is cached", ladder.walks, err)
			}
		})
	}
}

// TestAmbientCredentialHoldsAFailedWalkForTheBackoff: a failing ladder (a
// broken token command) is walked once per backoff rather than once per
// request, and while it fails no request goes out with the previous token.
func TestAmbientCredentialHoldsAFailedWalkForTheBackoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	previous := ambientCredentialNow
	ambientCredentialNow = func() time.Time { return now }
	t.Cleanup(func() { ambientCredentialNow = previous })

	helperFailed := errors.New("credential command failed: exit status 1")
	token, failure := "old", error(nil)
	ladder := &scriptedLadder{walk: func(int) (string, error) { return token, failure }}
	c := &resolvedAmbientCredential{inner: ladder}
	if header, err := authorizeHeader(t, c); err != nil || header != "Bearer old" {
		t.Fatalf("Authorize = %q, %v; want the ladder's token", header, err)
	}

	failure = helperFailed
	if retry, err := c.Refresh(context.Background()); retry || !errors.Is(err, helperFailed) {
		t.Fatalf("Refresh after a 401 = %v, %v; want the ladder's failure", retry, err)
	}
	for _, step := range []string{"a request", "a refresh", "a request just inside the backoff"} {
		if step == "a request just inside the backoff" {
			now = now.Add(ambientCredentialFailureBackoff - time.Nanosecond)
		}
		var err error
		var header string
		if step == "a refresh" {
			_, err = c.Refresh(context.Background())
		} else {
			header, err = authorizeHeader(t, c)
		}
		if !errors.Is(err, helperFailed) || header != "" {
			t.Fatalf("%s during the backoff = %q, %v; want the held failure and no Authorization", step, header, err)
		}
		if ladder.walks != 2 {
			t.Fatalf("%s during the backoff walked the ladder (walks %d, want 2)", step, ladder.walks)
		}
	}

	now = now.Add(time.Nanosecond)
	if _, err := authorizeHeader(t, c); !errors.Is(err, helperFailed) || ladder.walks != 3 {
		t.Fatalf("a request after the backoff = %v after %d walks; want one more walk, failing again", err, ladder.walks)
	}
	token, failure = "rotated", nil
	now = now.Add(ambientCredentialFailureBackoff)
	if header, err := authorizeHeader(t, c); err != nil || header != "Bearer rotated" || ladder.walks != 4 {
		t.Fatalf("a request after the ladder recovered = %q, %v after %d walks; want the new token from one more walk", header, err, ladder.walks)
	}
}
