package config

const (
	// PublicGastownPackSource is the concrete durable source for the wave-one
	// public gastown pack. Registry selectors resolve to this same concrete
	// source before being written to pack.toml.
	PublicGastownPackSource = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"

	// PublicGastownPackVersion pins fresh init output to the registry release
	// content commit from gastownhall/gascity-packs main.
	PublicGastownPackVersion = "sha:31382fc6dd86b747d19687f6028d8bcd85e059a7"
)
