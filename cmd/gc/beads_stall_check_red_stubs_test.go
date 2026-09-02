package main

import (
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

type stallCheckOptions struct {
	store        beads.Store
	mail         mail.Provider
	now          func() time.Time
	escalationTo string
	log          io.Writer
}

func runBeadsStallCheck(_ stallCheckOptions) error {
	return errStalledBeadNotImplemented
}
