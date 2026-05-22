package config

const (
	// PublicGastownPackSource is the concrete durable source for the wave-one
	// public gastown pack. Registry selectors resolve to this same concrete
	// source before being written to pack.toml.
	PublicGastownPackSource = "https://github.com/gastownhall/gascity-packs.git//gastown"

	// PublicGastownPackVersion pins fresh init output to the registry release
	// content commit from gastownhall/gascity-packs main.
	PublicGastownPackVersion = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
)
