package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// awaitProviderEntered waits until a blocking provider double reports that the
// first request reached it. A test that parks the first request inside the
// provider to observe an in-flight conflict must not wait on that signal alone:
// when the handler answers WITHOUT reaching the provider (the session read
// missed, the request was refused), the signal never comes and the test hangs
// until the package deadline, hiding every later failure. This wait also ends
// when the first request finishes, or after timeout, and reports why.
func awaitProviderEntered(entered <-chan struct{}, firstDone <-chan *httptest.ResponseRecorder, timeout time.Duration) (*httptest.ResponseRecorder, string) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-entered:
		return nil, ""
	case first := <-firstDone:
		return first, "the first request finished without reaching the provider"
	case <-timer.C:
		return nil, "the first request neither reached the provider nor finished within " + timeout.String()
	}
}

// requireProviderEntered is awaitProviderEntered for a test: it fails the test
// at once, with the first request's answer, instead of hanging.
func requireProviderEntered(t *testing.T, entered <-chan struct{}, firstDone <-chan *httptest.ResponseRecorder) {
	t.Helper()
	first, why := awaitProviderEntered(entered, firstDone, 30*time.Second)
	if why == "" {
		return
	}
	if first != nil {
		t.Fatalf("%s: status = %d, body = %s", why, first.Code, first.Body.String())
	}
	t.Fatal(why)
}

func TestAwaitProviderEnteredReturnsWhenTheProviderIsReached(t *testing.T) {
	entered := make(chan struct{})
	close(entered)
	if first, why := awaitProviderEntered(entered, make(chan *httptest.ResponseRecorder), time.Second); first != nil || why != "" {
		t.Fatalf("awaitProviderEntered = %v, %q; want success", first, why)
	}
}

func TestAwaitProviderEnteredFailsFastWhenTheFirstRequestFinishesFirst(t *testing.T) {
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusNotFound)
	firstDone <- rec
	start := time.Now()
	first, why := awaitProviderEntered(make(chan struct{}), firstDone, time.Minute)
	if first != rec || !strings.Contains(why, "without reaching the provider") {
		t.Fatalf("awaitProviderEntered = %v, %q; want the finished request and its reason", first, why)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("awaitProviderEntered took %s; it must not wait for its timeout once the request finished", elapsed)
	}
}

func TestAwaitProviderEnteredTimesOut(t *testing.T) {
	first, why := awaitProviderEntered(make(chan struct{}), make(chan *httptest.ResponseRecorder), 10*time.Millisecond)
	if first != nil || !strings.Contains(why, "neither reached the provider nor finished") {
		t.Fatalf("awaitProviderEntered = %v, %q; want a timeout", first, why)
	}
}
