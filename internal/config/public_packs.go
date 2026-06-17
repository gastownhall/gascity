package config

const (
	// PublicGastownPackSource is the concrete durable source for the wave-one
	// public gastown pack. Registry selectors resolve to this same concrete
	// source before being written to pack.toml.
	PublicGastownPackSource = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"

	// PublicGastownPackVersion pins fresh init output to the registry release
	// content commit from gastownhall/gascity-packs main.
	PublicGastownPackVersion = "sha:33d3a430a67d1782ad364556cb566bdb01d0afe3"

	// PublicGascityPackSource is the concrete durable source for the
	// gascity planning/implementation skills pack.
	PublicGascityPackSource = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"

	// PublicGascityPackVersion pins fresh init output to the registry
	// release content commit from gastownhall/gascity-packs main
	// (gascity 0.1.4).
	PublicGascityPackVersion = "sha:3b3b89f2011e06d84459aa7bea1552382f13930a"

	// BundledPackImportVersion pins the [imports.core]/[imports.bd] entries
	// gc init writes for the gascity.git packs bundled with the binary.
	// This is the CANONICAL pin: the only commit the binary pre-seeds into
	// the repo cache from its embedded content (in a binary-content-hashed
	// cache slot, so different binaries never fight over one entry). A
	// bundled source pinned at any other commit is an ordinary remote
	// import and is fetched from git for real — so editing a pin always
	// does what it says. The pin names a real gascity.git commit where the
	// bundled pack paths exist, keeping that fetch path honest.
	BundledPackImportVersion = "sha:f895c0ff47d6ee9334ed282a416387eb5b084d24"
)

// SupersededBundledPackImportVersions lists previous canonical pins for the
// gascity.git core/bd/dolt bundled packs, oldest first. Older gc releases
// wrote these as the canonical pin; the packv2-import-state doctor fix
// rewrites them to the current BundledPackImportVersion so a pin bump never
// strands a city on a network-only resolution path for content it only ever
// wanted as "the builtin". Deliberate user pins at other commits are
// untouched. When bumping BundledPackImportVersion, append the old value
// here.
var SupersededBundledPackImportVersions = []string{
	// Older Pack v2 lockfiles stored the bundled-pack content hash as the
	// canonical pin for gascity.git bundled packs. That value is not a git
	// commit, so current binaries must re-pin it offline instead of trying to
	// fetch it as an exact commit.
	"sha:282d2bf26b1a9396016e90b0128c1cd16b719f4d3af7cd0ea06cf25fbc426d18",
}

// SupersededPublicGastownPackVersions lists previous canonical pins for the
// public gastown pack, oldest first. Older gc releases and docs wrote these
// as "the canonical pin"; the packv2-import-state doctor fix rewrites them
// to the current PublicGastownPackVersion so a pin bump never strands a
// city on a network-only resolution path for content it only ever wanted
// as "the builtin". Deliberate user pins at other commits are untouched.
// When bumping PublicGastownPackVersion, append the old value here
// (scripts/update-bundled-gastown-pack does this).
var SupersededPublicGastownPackVersions = []string{
	"sha:4212acb7046c11f6f633df73307006493185233a",
	"sha:817f85e155e2b0b0c375835b076103108f8a4724",
	"sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b",
	"sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9",
}

// SupersededPublicGascityPackVersions is the gascity-pack counterpart of
// SupersededPublicGastownPackVersions.
var SupersededPublicGascityPackVersions = []string{
	"sha:99464ed9240b1f6e6b7ab1d351f67016e1a973ff",
	"sha:788b6e8ec224a8951c728ef6da74dab8bc04d474",
	"sha:5fc675b85d4ae0ebca2f17cb027a24b03f2832f8",
	"sha:abf24a2a123da29563f0473e6771e3f4769de0ab",
	"sha:af1640917a24f88126c37a1e3697a619b731cc0f",
}
