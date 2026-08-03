package productmetrics

// productionNoticeVersion is the monotonic version of the compiled first-run
// command-usage disclosure. Bump it whenever productionNoticeText changes in a
// way that requires re-consent: a higher version forces the notice to be shown
// and re-accepted before recording resumes.
const productionNoticeVersion = 1

// productionNoticeText is the owner-approved first-run disclosure shown once on
// an eligible interactive TTY before any command-usage event is recorded. It
// states what is collected, that collection is on by default, and every opt-out
// path. It intentionally references no external URL.
const productionNoticeText = `Gas City collects anonymous usage metrics from the gc command line to
understand how gc is used and where to improve it.

What's collected: the command name, the gc version, your operating system,
and an anonymous installation ID. Nothing else — no command arguments, file
names or contents, paths, environment values, IP addresses, or personal data.

This is enabled by default. You can turn it off at any time:
  • run:  gc metrics off
  • or set  DO_NOT_TRACK=1  or  GC_DISABLE_USAGE_METRICS=1

Metrics are never collected in CI, scripted, or agent-managed sessions, and
this first run has not been recorded.
`

// productionNotice returns the compiled production notice wired into every real
// artifact. It is not a test-only notice; the notice and upload gates accept it
// because it is complete — a non-zero version and non-empty text.
func productionNotice() noticeDefinition {
	return noticeDefinition{
		version: productionNoticeVersion,
		text:    []byte(productionNoticeText),
	}
}
