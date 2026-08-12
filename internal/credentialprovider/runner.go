package credentialprovider

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

const (
	maxStdoutBytes   = 64 << 10
	maxStderrBytes   = 8 << 10
	commandWaitDelay = time.Second
	commandKillGrace = 250 * time.Millisecond
)

type commandControl struct {
	afterStart func() error
	cancel     func() error
	close      func() error
}

func runCommand(ctx context.Context, argv []string, stdin []byte, environment []string) (commandOutput, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = make([]string, len(environment))
	copy(cmd.Env, environment)
	cmd.Stdin = bytes.NewReader(stdin)
	stdout := boundedBuffer{limit: maxStdoutBytes}
	stderr := boundedBuffer{limit: maxStderrBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = commandWaitDelay

	control, err := newCommandControl(cmd)
	if err != nil {
		return commandOutput{}, err
	}
	cmd.Cancel = func() error {
		_ = control.cancel()
		return nil
	}
	if err := cmd.Start(); err != nil {
		return outputFromBuffers(&stdout, &stderr), errors.Join(err, control.close())
	}
	if err := control.afterStart(); err != nil {
		cancelErr := control.cancel()
		waitErr := cmd.Wait()
		return outputFromBuffers(&stdout, &stderr), errors.Join(err, cancelErr, waitErr, control.close())
	}
	waitErr := cmd.Wait()
	return outputFromBuffers(&stdout, &stderr), errors.Join(waitErr, control.close())
}

func outputFromBuffers(stdout, stderr *boundedBuffer) commandOutput {
	return commandOutput{
		stdout:         stdout.bytes(),
		stderr:         stderr.bytes(),
		stdoutOverflow: stdout.overflowed(),
		stderrOverflow: stderr.overflowed(),
	}
}

type boundedBuffer struct {
	buffer   []byte
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := b.limit - len(b.buffer)
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		b.buffer = append(b.buffer, data[:remaining]...)
	}
	if remaining < len(data) {
		b.overflow = true
	}
	return written, nil
}

func (b *boundedBuffer) bytes() []byte {
	return append([]byte(nil), b.buffer...)
}

func (b *boundedBuffer) overflowed() bool {
	return b.overflow
}
