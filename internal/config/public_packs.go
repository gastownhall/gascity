package config

const (
	// PublicGastownPackSource is the concrete durable source for the wave-one
	// public gastown pack. Registry selectors resolve to this same concrete
	// source before being written to pack.toml.
	PublicGastownPackSource = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"

	// PublicGastownPackVersion pins fresh init output to the registry release
	// content commit from gastownhall/gascity-packs main.
	PublicGastownPackVersion = "sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9"

	// PublicGascityPackSource is the concrete durable source for the
	// gascity planning/implementation skills pack.
	PublicGascityPackSource = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"

	// PublicGascityPackVersion pins fresh init output to the registry
	// release content commit from gastownhall/gascity-packs main.
	PublicGascityPackVersion = "sha:5fc675b85d4ae0ebca2f17cb027a24b03f2832f8"
)
