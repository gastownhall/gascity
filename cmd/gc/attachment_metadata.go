package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sling"
)

func attachmentLabel(b beads.Bead) string {
	return sling.AttachmentLabel(b)
}
