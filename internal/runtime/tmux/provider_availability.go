package tmux

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.ProviderAvailabilityObserver = (*Provider)(nil)

// ObserveProviderAvailability diagnoses the current Codex UI without touching
// input, lifecycle or native turn records. The existing input fence is read,
// never adopted: both captures must name the exact same live agent pane and
// incarnation. An uncertain read is unknown, never ready or a stale failure.
func (p *Provider) ObserveProviderAvailability(ctx context.Context, name string, req runtime.ProviderAvailabilityRequest) *runtime.ProviderAvailability {
	if req.Family != "codex" {
		return nil
	}
	observation := &runtime.ProviderAvailability{State: "unknown", Source: "native_ui", ObservedAt: time.Now().UTC()}
	if req.Fence.Validate() != nil || !validSessionNameRe.MatchString(name) {
		return observation
	}
	observation.RuntimeGeneration = req.Fence.Digest()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	guard := newPaneGuard(name, req.Fence.Digest(), false)
	target, before, err := guard.read(ctx, p.tm)
	if err != nil || before.Dead || before.Own() != req.Fence.Digest() {
		return observation
	}
	// No -S: historical scrollback is not a live failure widget. The target
	// is already the exact agent pane resolved by the existing fence read.
	screen, err := p.tm.runCtxRaw(ctx, "capture-pane", "-p", "-t", target)
	if err != nil || len(screen) > 64<<10 {
		return observation
	}
	afterTarget, after, err := guard.read(ctx, p.tm)
	if err != nil || target != afterTarget || before.Pane != after.Pane || after.Dead || after.Own() != req.Fence.Digest() {
		return observation
	}
	if codexConnectionRestoreFailed(screen) {
		observation.State = "unavailable"
		observation.Code = "connection_restore_failed"
	}
	observation.ObservedAt = time.Now().UTC()
	return observation
}

var (
	availabilityANSI         = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	codexRestoreFailedStatus = regexp.MustCompile(`^• Reconnect failed — check the endpoint, then relaunch \([0-9hms ]+\)$`)
)

type availabilityUIBlock struct {
	text   string
	end    int
	quoted bool
}

// codexConnectionRestoreFailed recognizes the complete current native widget,
// including its current relaunch/timer row and the native composer/footer. It
// deliberately recognizes no generic error words, retry notices, quoted tool
// examples or old error followed by a later assistant turn.
func codexConnectionRestoreFailed(screen string) bool {
	lines := strings.Split(strings.ReplaceAll(availabilityANSI.ReplaceAllString(screen, ""), "\r", ""), "\n")
	var blocks []availabilityUIBlock
	var words []string
	quoted, blockQuoted := false, false
	flush := func(end int) {
		if len(words) > 0 {
			blocks = append(blocks, availabilityUIBlock{text: strings.Join(words, " "), end: end, quoted: blockQuoted})
			words = nil
			blockQuoted = false
		}
	}
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			quoted = !quoted
			blockQuoted = true
		}
		if strings.TrimSpace(line) == "" {
			flush(i)
			continue
		}
		if len(words) == 0 {
			// Preserve a native block's column-zero marker. Wrapped text
			// below it can be joined without allowing an indented example.
			words = append(words, strings.TrimRight(line, " \t"))
		} else {
			words = append(words, strings.TrimSpace(line))
		}
		blockQuoted = blockQuoted || quoted
	}
	flush(len(lines))
	for i := 0; i+2 < len(blocks); i++ {
		heading, failure, status := blocks[i], blocks[i+1], blocks[i+2]
		if heading.quoted || failure.quoted || status.quoted {
			continue
		}
		if heading.text != "■ Automatic reconnect could not restore this session. Your draft is still editable. Copy it before quitting with Ctrl+C, then reconnect with the same command." ||
			failure.text != "■ Server connection could not be restored" ||
			!codexRestoreFailedStatus.MatchString(status.text) {
			continue
		}
		if codexRestoreFailureFooter(lines[status.end:]) {
			return true
		}
	}
	return false
}

func codexRestoreFailureFooter(lines []string) bool {
	var nonempty []string
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonempty = append(nonempty, strings.TrimRight(line, " \t"))
		}
	}
	if len(nonempty) < 3 || (nonempty[0] != "›" && !strings.HasPrefix(nonempty[0], "› ")) || strings.TrimSpace(nonempty[len(nonempty)-1]) != "⌃c quit" {
		return false
	}
	modelFooter := false
	for _, line := range nonempty[1 : len(nonempty)-1] {
		if strings.HasPrefix(line, "• ") || strings.HasPrefix(line, "■ ") || strings.HasPrefix(line, "```") {
			return false
		}
		modelFooter = modelFooter || (strings.HasPrefix(line, "  ") && strings.Contains(line, " · "))
	}
	return modelFooter
}
