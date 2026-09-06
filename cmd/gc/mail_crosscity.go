package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
)

// mailCityRosterFor builds the cross-city mail roster for the loaded city.
// The zero roster (no [mail.crosscity] section, or no loadable config) is
// disabled and leaves every recipient resolving exactly as today.
func mailCityRosterFor(cfg *config.City, cityPath string) mail.CityRoster {
	local, peers := cfg.MailCityRoster(loadedCityName(cfg, cityPath))
	return mail.CityRoster{Local: local, Peers: peers}
}

// crossCityNotifyRefusal is the typed refusal for --notify on a recipient in
// another city. A cross-machine wake primitive does not exist; the
// recipient's wake is its own city's mail sweep, so the request is refused
// loudly before any write rather than reporting a nudge into a leg that is
// not there.
func crossCityNotifyRefusal(cmdName, recipient string) string {
	return fmt.Sprintf("%s: --notify does not cross cities: %q is in city %q; its wake is that city's own mail sweep. Retry without --notify.",
		cmdName, recipient, strings.SplitN(recipient, "/", 2)[0])
}
