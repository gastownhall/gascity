package config

const (
	// PublicGastownPackSource is the concrete durable source for the wave-one
	// public gastown pack. Registry selectors resolve to this same concrete
	// source before being written to pack.toml.
	PublicGastownPackSource = "https://github.com/gastownhall/gascity-packs.git//gastown"

	// PublicGastownPackVersion pins fresh init output to the validation commit
	// from the public built-in pack migration branch.
	PublicGastownPackVersion = "sha:0090440b8e8efa4c40c2cef6bf585805ac87fa37"
)
