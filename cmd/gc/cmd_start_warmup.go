package main

import (
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/mail"
)

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
	return newCommandMailProviderNamed(name, store)
}
