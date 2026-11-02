package tmux

type processSignal string

const (
	processSignalTerm processSignal = "TERM"
	processSignalKill processSignal = "KILL"
)
