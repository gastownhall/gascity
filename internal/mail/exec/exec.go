// Package exec implements [mail.Provider] by delegating each operation to
// a user-supplied script via fork/exec. This follows the Git credential
// helper pattern: a single script receives the operation name as its first
// argument and communicates via JSON on stdin/stdout.
package exec //nolint:revive // internal package, always imported with alias

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
)

// Provider implements [mail.Provider] by delegating to a user-supplied script.
type Provider struct {
	script  string
	timeout time.Duration
	ready   sync.Once // ensure-running called once
}

// NewProvider returns an exec mail provider that delegates to the given script.
func NewProvider(script string) *Provider {
	return &Provider{
		script:  script,
		timeout: 30 * time.Second,
	}
}

// Send delegates to: script send <to> with JSON {"from":"...","subject":"...","body":"..."} on stdin.
func (p *Provider) Send(from, to, subject, body string) (mail.Message, error) {
	p.ensureRunning()
	data, err := marshalSendInput(from, subject, body)
	if err != nil {
		return mail.Message{}, err
	}
	out, err := p.run(data, "send", to)
	if err != nil {
		return mail.Message{}, err
	}
	return unmarshalMessage(out)
}

// Inbox delegates to: script inbox <recipient>
func (p *Provider) Inbox(recipient string) ([]mail.Message, error) {
	p.ensureRunning()
	out, err := p.run(nil, "inbox", recipient)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return unmarshalMessages(out)
}

// Get delegates to: script get <id>
func (p *Provider) Get(id string) (mail.Message, error) {
	p.ensureRunning()
	out, err := p.run(nil, "get", id)
	if err != nil {
		return mail.Message{}, err
	}
	return unmarshalMessage(out)
}

// Read delegates to: script read <id>
func (p *Provider) Read(id string) (mail.Message, error) {
	p.ensureRunning()
	out, err := p.run(nil, "read", id)
	if err != nil {
		return mail.Message{}, err
	}
	return unmarshalMessage(out)
}

// MarkRead delegates to: script mark-read <id>
func (p *Provider) MarkRead(id string) error {
	p.ensureRunning()
	_, err := p.run(nil, "mark-read", id)
	return err
}

// MarkUnread delegates to: script mark-unread <id>
func (p *Provider) MarkUnread(id string) error {
	p.ensureRunning()
	_, err := p.run(nil, "mark-unread", id)
	return err
}

// Archive delegates to: script archive <id>
// If the script writes "already archived" to stderr and exits non-zero,
// the error wraps [mail.ErrAlreadyArchived].
func (p *Provider) Archive(id string) error {
	p.ensureRunning()
	_, err := p.run(nil, "archive", id)
	if err != nil && strings.Contains(err.Error(), "already archived") {
		return fmt.Errorf("exec mail archive: %w", mail.ErrAlreadyArchived)
	}
	return err
}

// Delete delegates to: script delete <id>
func (p *Provider) Delete(id string) error {
	p.ensureRunning()
	_, err := p.run(nil, "delete", id)
	if err != nil && strings.Contains(err.Error(), "already archived") {
		return fmt.Errorf("exec mail delete: %w", mail.ErrAlreadyArchived)
	}
	return err
}

// ArchiveMany archives a batch by looping over [Provider.Archive].
// The exec script protocol is single-id per invocation; a batch endpoint
// would require a protocol extension that is out of scope here.
func (p *Provider) ArchiveMany(ids []string) ([]mail.ArchiveResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	results := make([]mail.ArchiveResult, len(ids))
	for i, id := range ids {
		results[i] = mail.ArchiveResult{ID: id, Err: p.Archive(id)}
	}
	return results, nil
}

// DeleteMany deletes a batch by looping over [Provider.Delete].
// The exec script protocol is single-id per invocation; a batch endpoint
// would require a protocol extension that is out of scope here.
func (p *Provider) DeleteMany(ids []string) ([]mail.ArchiveResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	results := make([]mail.ArchiveResult, len(ids))
	for i, id := range ids {
		results[i] = mail.ArchiveResult{ID: id, Err: p.Delete(id)}
	}
	return results, nil
}

// All delegates to: script all <recipient>
func (p *Provider) All(recipient string) ([]mail.Message, error) {
	p.ensureRunning()
	out, err := p.run(nil, "all", recipient)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return unmarshalMessages(out)
}

// Check delegates to: script check <recipient>
func (p *Provider) Check(recipient string) ([]mail.Message, error) {
	p.ensureRunning()
	out, err := p.run(nil, "check", recipient)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return unmarshalMessages(out)
}

// Reply delegates to: script reply <id> with JSON {"from":"...","subject":"...","body":"..."} on stdin.
func (p *Provider) Reply(id, from, subject, body string) (mail.Message, error) {
	p.ensureRunning()
	data, err := marshalReplyInput(from, subject, body)
	if err != nil {
		return mail.Message{}, err
	}
	out, err := p.run(data, "reply", id)
	if err != nil {
		return mail.Message{}, err
	}
	return unmarshalMessage(out)
}

// Thread delegates to: script thread <id>, where id may be a thread ID or
// any message ID in that thread.
func (p *Provider) Thread(id string) ([]mail.Message, error) {
	p.ensureRunning()
	out, err := p.run(nil, "thread", id)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return unmarshalMessages(out)
}

// Count delegates to: script count <recipient>
func (p *Provider) Count(recipient string) (int, int, error) {
	p.ensureRunning()
	out, err := p.run(nil, "count", recipient)
	if err != nil {
		return 0, 0, err
	}
	if out == "" {
		return 0, 0, nil
	}
	return unmarshalCount(out)
}

// ensureRunning calls "ensure-running" on the script once per provider
// lifetime. Exit 2 (unknown op) is treated as success.
func (p *Provider) ensureRunning() {
	p.ready.Do(func() {
		_, _ = p.run(nil, "ensure-running")
	})
}

// run executes the script with the given args, optionally piping stdinData
// to its stdin. Returns the trimmed stdout on success.
//
// Exit code 2 is treated as success (unknown operation — forward compatible).
// Any other non-zero exit code returns an error wrapping stderr.
func (p *Provider) run(stdinData []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.script, args...)
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if stdinData != nil {
		cmd.Stdin = bytes.NewReader(stdinData)
	}

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 2 {
				return "", nil
			}
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("exec mail provider %s %s: %s", p.script, strings.Join(args, " "), errMsg)
	}

	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Compile-time interface check.
var _ mail.Provider = (*Provider)(nil)
