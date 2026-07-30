package redstreak

import (
	"errors"

	"github.com/gastownhall/gascity/internal/config"
)

// EvaluateHistory reconstructs the consecutive red/green streak of a
// monitor's aggregate job from its observed run history.
func EvaluateHistory(monitor config.GitHubRunMonitor, runs []ObservedRun) (Evaluation, error) {
	_, _ = monitor, runs
	return Evaluation{}, errors.New("not implemented")
}
