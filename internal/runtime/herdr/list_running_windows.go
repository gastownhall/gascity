//go:build windows

package herdr

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

func listRunningFailure(err error) error {
	if herdrCodeAnyShape(err) == "server_not_running" {
		return &runtime.PartialListError{Err: fmt.Errorf("herdr server unreachable: %w", err), ServerAbsent: true}
	}
	return err
}
