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

	// BundledPackImportVersion pins the [imports.core]/[imports.bd] entries
	// gc init writes for the packs bundled with the binary. The bundled
	// synthetic repo cache serves the RUNNING BINARY's embedded content for
	// any requested commit (the cache key folds in the binary content hash),
	// so this pin is primarily a stable cache/lock tag; it names a real
	// gascity.git commit where the bundled pack paths exist so the git
	// fallback degrades gracefully when a binary cannot serve the source.
	BundledPackImportVersion = "sha:f895c0ff47d6ee9334ed282a416387eb5b084d24"
)
