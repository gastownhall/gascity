package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/mailtest"
)

func TestBeadmailConformance(t *testing.T) {
	mailtest.RunProviderTests(t, func(_ *testing.T) mail.Provider {
		return newTestProvider(beads.NewMemStore())
	})
}

func TestBeadmailReplyToOwnMessage(t *testing.T) {
	mailtest.RunReplyToOwnMessageTests(t, func(_ *testing.T) mail.Provider {
		return newTestProvider(beads.NewMemStore())
	})
}
