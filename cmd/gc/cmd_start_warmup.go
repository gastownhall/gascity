package main

import (
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/warmup"
)

// startWarmupOpts is the warm-up runner configuration `gc start` uses. The
// failure mail goes to [doctor] warmup_mail_to, or to the runner's default
// recipient when the city leaves it unset.
func startWarmupOpts(cfg *config.City, checks []doctor.Check, mailer mail.Provider, stderr io.Writer) warmup.WarmupOpts {
	opts := warmup.WarmupOpts{
		Checks: checks,
		Mailer: mailer,
		Stderr: stderr,
	}
	if cfg != nil {
		opts.MailTo = cfg.Doctor.WarmupMailTo
	}
	return opts
}

// defaultMailProvider builds the mailer `gc start` hands the warm-up runner,
// for the city being started rather than the one discovery would find.
//
// Relocation: a warm-up mail is a type=message bead, which internal/coordclass
// classifies messaging — an INFRASTRUCTURE class. On a converged split city it
// belongs in the binding, and writing it to the work store is the write the
// next boot's containment re-check reads as a stranded infrastructure bead and
// refuses to start over. This runs BEFORE the boot gate, so the routing cannot
// wait for CityRuntime: it takes the same three-valued verdict from the same
// gate through the one-shot funnel, exactly as `gc mail` does
// (openCityMailProvider, providers.go). Serve puts the mail in the binding,
// bypass leaves it on the work store byte-identically, and refuse fails the
// Send — which warm-up already tolerates, because RunWarmupChecks records
// MailSendError and continues.
func defaultMailProvider(cityPath string) mail.Provider {
	name := os.Getenv("GC_MAIL")
	if name == "" {
		name = mailProviderNameForCity(cityPath)
	}
	if strings.HasPrefix(name, "exec:") || name == "fake" || name == "fail" {
		return newCommandMailProviderNamed(name, nil)
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil
	}
	// A nil cfg is the resolvers' identity input: where a city serves its
	// coordination classes from is a property of the CITY, and cliStorageRoutes
	// reads its city.toml itself.
	msgStore := resolveMailMessagesStore(cliStorageRoutes(cityPath), store, nil, cityPath, nil)
	sessStore := cliSessionStore(store, nil, cityPath)
	return newMailProviderNamedWithSessionStore(name, msgStore, sessStore, true)
}
