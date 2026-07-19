//go:build integration

package dashport_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/test/dashport/corpus"
)

// The corpus package is the single source of truth for the seeded scenario;
// these aliases keep the projection assertions reading short local names while
// the ids/values live in exactly one place (shared with the Playwright fake
// supervisor).
const (
	corpusCityName     = corpus.CityName
	corpusRigName      = corpus.RigName
	anchorRunID        = corpus.AnchorRunID
	anchorStepID       = corpus.AnchorStepID
	anchorFormula      = corpus.AnchorFormula
	completedRunID     = corpus.CompletedRunID
	completedFormula   = corpus.CompletedFormula
	completedStepA     = corpus.CompletedStepAnalyzeID
	completedStepB     = corpus.CompletedStepApproveID
	corpusSourceBeadID = corpus.SourceBeadID
	corpusWorkBeadID   = corpus.WorkBeadID
	corpusWorkBeadName = corpus.WorkBeadTitle
	corpusMailSubject  = corpus.MailSubject

	anchorStepTitle       = corpus.AnchorStepTitle
	anchorReviewStepID    = corpus.AnchorReviewStepID
	corpusAgentSlug       = corpus.AgentSessionSlug
	corpusAgentTemplate   = corpus.AgentSessionTemplate
	corpusOperatorSubject = corpus.OperatorMailSubject
	corpusOperatorBody    = corpus.OperatorMailBody
	corpusAgentReplyBody  = corpus.AgentReplyBody
)

// loadFixtures seeds a city from testdata/dashport via the shared corpus loader
// and registers cleanup on t. It is a thin t.Helper wrapper: the seeding logic
// lives once in test/dashport/corpus so the same seeded state backs both this
// serve-level test (Layer A) and the Playwright fake supervisor (Layer B). A
// load error fails the test rather than returning, preserving the previous
// t.Fatal behavior.
func loadFixtures(t *testing.T) *corpus.Fixtures {
	t.Helper()

	fx, err := corpus.Load(corpusDataDir(t), t.TempDir())
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	t.Cleanup(func() { _ = fx.Close() })
	return fx
}

// corpusDataDir resolves the testdata/dashport directory relative to this test
// package's working directory (the package dir under `go test`).
func corpusDataDir(t *testing.T) string {
	t.Helper()
	return "testdata/dashport"
}

// serveSeededCity wires the loaded corpus into the exported production seam.
// The returned stop function drains the plane's run tailers and status samplers.
func serveSeededCity(ctx context.Context, fx *corpus.Fixtures) (http.Handler, func(), error) {
	return api.ServeSeededCity(ctx, api.SeededCityDeps{
		CityName:      fx.CityName,
		CityPath:      fx.CityPath,
		Config:        fx.Config,
		CityBeadStore: fx.CityStore,
		RigStores:     fx.RigStores,
		MailProvider:  fx.MailProv,
		EventProvider: fx.EventProv,
	}, "")
}
