package beads

import (
	"context"
	"errors"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

type nativeIDCensusStorageSpy struct {
	*nativeDoltStorageSpy
	searchIssueIDs func(context.Context, string, beadslib.IssueFilter) ([]string, error)
}

func (s *nativeIDCensusStorageSpy) SearchIssueIDs(ctx context.Context, query string, filter beadslib.IssueFilter) ([]string, error) {
	if s.searchIssueIDs == nil {
		return nil, nil
	}
	return s.searchIssueIDs(ctx, query, filter)
}

func TestNativeDoltStoreScanAllIDsIncludesMalformedMetadataWithoutParsing(t *testing.T) {
	idSearchCalls := 0
	wideSearchCalls := 0
	storage := &nativeIDCensusStorageSpy{
		nativeDoltStorageSpy: &nativeDoltStorageSpy{
			searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
				wideSearchCalls++
				return nil, errors.New("unexpected wide SearchIssues")
			},
		},
		searchIssueIDs: func(_ context.Context, query string, filter beadslib.IssueFilter) ([]string, error) {
			idSearchCalls++
			if query != "" {
				t.Fatalf("SearchIssueIDs query = %q, want complete scan", query)
			}
			if filter.Status != nil || len(filter.ExcludeStatus) != 0 || filter.Ephemeral != nil || filter.Limit != 0 || filter.IncludeDependencies || len(filter.IDs) != 0 || filter.SkipWisps {
				t.Fatalf("SearchIssueIDs filter = %#v, want unbounded all-status/all-tier ID scan", filter)
			}
			// gc-corrupt represents a row whose metadata cannot be decoded by List.
			// The ID-only projection must retain it for authoritative hydration.
			return []string{"gc-valid", "gc-corrupt", "gc-valid"}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.ScanAllIDs()
	if err != nil {
		t.Fatalf("ScanAllIDs: %v", err)
	}
	if want := []string{"gc-valid", "gc-corrupt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanAllIDs = %v, want %v", got, want)
	}
	if idSearchCalls != 1 || wideSearchCalls != 0 {
		t.Fatalf("SearchIssueIDs calls = %d, SearchIssues calls = %d, want 1, 0", idSearchCalls, wideSearchCalls)
	}
}

func TestNativeDoltStoreScanAllIDsFailsOnMalformedIdentity(t *testing.T) {
	storage := &nativeIDCensusStorageSpy{
		nativeDoltStorageSpy: &nativeDoltStorageSpy{},
		searchIssueIDs: func(context.Context, string, beadslib.IssueFilter) ([]string, error) {
			return []string{"gc-valid", " "}, nil
		},
	}
	got, err := newNativeDoltStoreForTest(storage).ScanAllIDs()
	if err == nil {
		t.Fatal("ScanAllIDs error = nil, want strict identity failure")
	}
	if got != nil {
		t.Fatalf("ScanAllIDs = %v, want nil on failure", got)
	}
}

func TestNativeDoltStoreScanAllIDsFailsWholeCensusOnSearchError(t *testing.T) {
	cause := errors.New("ID projection unavailable")
	storage := &nativeIDCensusStorageSpy{
		nativeDoltStorageSpy: &nativeDoltStorageSpy{},
		searchIssueIDs: func(context.Context, string, beadslib.IssueFilter) ([]string, error) {
			return []string{"gc-partial"}, cause
		},
	}
	got, err := newNativeDoltStoreForTest(storage).ScanAllIDs()
	if !errors.Is(err, cause) {
		t.Fatalf("ScanAllIDs error = %v, want errors.Is(%v)", err, cause)
	}
	if got != nil {
		t.Fatalf("ScanAllIDs = %v, want nil on search error", got)
	}
}

var _ beadslib.Storage = (*nativeIDCensusStorageSpy)(nil)
