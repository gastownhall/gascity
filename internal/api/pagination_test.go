package api

import (
	"net/http/httptest"
	"testing"
)

func TestParsePagination_LimitZeroMeansAll(t *testing.T) {
	req := httptest.NewRequest("GET", "/v0/beads?limit=0", nil)
	pp := parsePagination(req, 50)
	if pp.Limit != maxPaginationLimit {
		t.Errorf("limit=0 should mean no limit (%d), got %d", maxPaginationLimit, pp.Limit)
	}
}

func TestParsePagination_DefaultLimit(t *testing.T) {
	req := httptest.NewRequest("GET", "/v0/beads", nil)
	pp := parsePagination(req, 50)
	if pp.Limit != 50 {
		t.Errorf("default limit should be 50, got %d", pp.Limit)
	}
}

func TestParsePagination_ExplicitLimit(t *testing.T) {
	req := httptest.NewRequest("GET", "/v0/beads?limit=25", nil)
	pp := parsePagination(req, 50)
	if pp.Limit != 25 {
		t.Errorf("explicit limit=25 should be 25, got %d", pp.Limit)
	}
}

func TestParsePagination_NegativeLimitUsesDefault(t *testing.T) {
	req := httptest.NewRequest("GET", "/v0/beads?limit=-5", nil)
	pp := parsePagination(req, 50)
	if pp.Limit != 50 {
		t.Errorf("negative limit should fall back to default 50, got %d", pp.Limit)
	}
}
