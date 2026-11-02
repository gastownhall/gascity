package config

const (
	// PublicGastownPackSource is the concrete durable source for the wave-one
	// public gastown pack. Registry selectors resolve to this same concrete
	// source before being written to pack.toml.
	PublicGastownPackSource = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"

	// PublicGastownPackVersion pins fresh init output to the registry release
	// content commit from gastownhall/gascity-packs (main, or a gastown
	// release branch such as release/gastown-0.1.11).
	PublicGastownPackVersion = "sha:cc30e82bafe81bff6a12a9c52f8de3df3c89d9f4"

	// PublicGascityPackSource is the concrete durable source for the
	// gascity planning/implementation skills pack.
	PublicGascityPackSource = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"

	// PublicGascityPackVersion pins fresh init output to the registry
	// release content commit from gastownhall/gascity-packs main
	// (RFC8191 candidate branch; qualify this exact content before release).
	PublicGascityPackVersion = "sha:785f4bd6660c7d7e756bbcfcbe14cc514904434c"

	// PublicGascityRolesPackSource is the concrete durable source for the
	// gascity role-agents subpack (gc-roles): the providerless, rig-scoped
	// agents (run-operator, requirements-planner, design-author, ...) that the
	// gascity formulas route to. It lives in the same repo as
	// PublicGascityPackSource and is pinned to the same release commit
	// (PublicGascityPackVersion), so a fresh gascity city's formulas and the
	// rig roles they coordinate always come from one matching release.
	//
	// It is a bundled subpack of the gascity pack (builtinpacks
	// bundledSubpacks): at PublicGascityPackVersion it resolves offline from
	// the gascity pack's embedded tree, in the same synthetic cache directory,
	// so gc init and gc rig add never clone gascity-packs for it (ga-73eoo).
	PublicGascityRolesPackSource = "https://github.com/gastownhall/gascity-packs/tree/main/gascity/roles"

	// BundledPackImportVersion pins the [imports.core]/[imports.bd] entries
	// gc init writes for the gascity.git packs bundled with the binary.
	// This is the CANONICAL pin: the only commit the binary pre-seeds into
	// the repo cache from its embedded content (in a binary-content-hashed
	// cache slot, so different binaries never fight over one entry). A
	// bundled source pinned at any other commit is an ordinary remote
	// import and is fetched from git for real — so editing a pin always
	// does what it says. The pin names a real gascity.git commit whose
	// bundled pack trees match the content this binary embeds, so a gc that
	// does not know the pin (an older release reading a newer city) fetches
	// the same bytes. scripts/check-embedded-pins (RC Gate) checks that the
	// commit is an ancestor of the release ref with identical core/bd/dolt
	// trees; bump the pin (and append the old value to
	// SupersededBundledPackImportVersions) when cutting a release whose
	// bundled core/bd/dolt content changed.
	BundledPackImportVersion = "sha:9dedf0ce789ce63133cfe7ff6b81fb2be2f59d8a"
)

// SupersededBundledPackImportVersions lists previous canonical pins for the
// gascity.git core/bd/dolt bundled packs, oldest first. Older gc releases
// wrote these as the canonical pin for content they only ever meant as "the
// builtin", so the running binary keeps serving its embedded content for
// them (never a git fetch of the old commit), and the packv2-import-state
// doctor fix rewrites them to the current BundledPackImportVersion.
// Deliberate user pins at other commits are untouched. When bumping
// BundledPackImportVersion, append the old value here.
var SupersededBundledPackImportVersions = []string{
	// Older Pack v2 lockfiles stored the bundled-pack content hash as the
	// canonical pin for gascity.git bundled packs. That value is not a git
	// commit, so current binaries must re-pin it offline instead of trying to
	// fetch it as an exact commit.
	"sha:282d2bf26b1a9396016e90b0128c1cd16b719f4d3af7cd0ea06cf25fbc426d18",
	// The June 2026 marker commit every gc through v1.5.0 wrote. Its tree
	// predates the content those binaries embedded, so it is served from
	// embedded content and never fetched (isSupersededEmbeddedOnlyPin).
	"sha:f895c0ff47d6ee9334ed282a416387eb5b084d24",
	"sha:e38ce9cc55d15fa351199c042f0d07aba1a347af",
	"sha:b21ab3b27bd09f9130fb457e6fbf9e3570ea7297",
	"sha:bca762adc8ef16fdc7de301b6b31b5ae4dfe2607",
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
	"sha:cb6a612882b8402569c495c90ab75bb257a51bdc",
	"sha:66c672b5ed00af143158b4eac960523c56b977ee",
	"sha:33d3a430a67d1782ad364556cb566bdb01d0afe3",
	"sha:4212acb7046c11f6f633df73307006493185233a",
	"sha:817f85e155e2b0b0c375835b076103108f8a4724",
	"sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b",
	"sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9",
	"sha:342bcfb0775ad79d2c67df3b235edf70a0a7e372",
	"sha:2cd3360f36b2ff55f6c306546841963cbca1ed69",
	"sha:92a9e8558b86854264ccc082fe9f27d48db3c749",
	"sha:124902097faa3d2e4de69e5ac5fe9e5109cad24f",
	"sha:478292cb99aec2efe4b3cecc636677b4d2af9665",
	"sha:cb068845a82e033c32a445d6011ad7127c97377c",
	"sha:9ea1e00ca15aa4e793b62b6be1fb05d86e3a1212",
	"sha:50e6ce8fc07eee263a10aa9b138bca919f8d584e",
	"sha:0decca6b7ea4f0720a358bc244f53d1db32bfdb6",
	"sha:c27b62ad047b7250fd35e7382e953d9478a1b174",
}

// SupersededPublicGascityPackVersions is the gascity-pack counterpart of
// SupersededPublicGastownPackVersions.
var SupersededPublicGascityPackVersions = []string{
	"sha:3b3b89f2011e06d84459aa7bea1552382f13930a",
	"sha:99464ed9240b1f6e6b7ab1d351f67016e1a973ff",
	"sha:788b6e8ec224a8951c728ef6da74dab8bc04d474",
	"sha:5fc675b85d4ae0ebca2f17cb027a24b03f2832f8",
	"sha:abf24a2a123da29563f0473e6771e3f4769de0ab",
	"sha:af1640917a24f88126c37a1e3697a619b731cc0f",
	"sha:39f07fed3524c016482b82fa0d4973aa6b4fc05e",
	"sha:7aedf80cfa39905bee4104095bfae8a02c67aaa1",
	"sha:20ac017f5417b82a300f0bfdfa7cddab3773cb07",
	"sha:ef528014c0fd6ec9d2bd6eded4fe800cf61758bc",
	"sha:124902097faa3d2e4de69e5ac5fe9e5109cad24f",
	"sha:676381ee880a38adb9a629b2cffdb17dd8676243",
	"sha:9b335f3d6e1326c2230c4d9037ef6e2352a51ba8",
	"sha:fba9d21174c59bc363fc21b2097092cdc8200933",
	"sha:1c7b7c7f1e9799b78b84b11780e1012a70fd3ae9",
	"sha:a1c6300426246931c87ffe35b796c4e102982a40",
	"sha:4c037d340f4b3bb537d60f865143f2015de6f873",
}
