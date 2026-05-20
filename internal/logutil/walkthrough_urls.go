package logutil

// WalkthroughURL maps fatal failure-mode keys to their canonical docs URLs.
var WalkthroughURL = map[string]string{
	"bd_op_init_timeout":   "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#bd-op-init-timeout",
	"pack_schema_mismatch": "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#pack-schema-mismatch",
	"duplicate_name_v1v2":  "https://docs.gascityhall.com/guides/migrating-to-pack-vnext#agents",
	"duplicate_name_other": "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#duplicate-name",
	"unknown_field":        "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#unknown-field-agent-pool",
	"rig_path_required":    "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#rig-path-required",
	"template_not_found":   "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#template-not-found",
	"duplicate_identity":   "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#duplicate-identity",
}
