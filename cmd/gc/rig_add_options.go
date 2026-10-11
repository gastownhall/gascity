package main

// rigAddOptions carries the low-traffic `gc rig add` flags that do not earn
// another positional parameter on the already-wide doRigAdd signature.
type rigAddOptions struct {
	allowEphemeralPath bool
}

// rigAddOption mutates rigAddOptions.
type rigAddOption func(*rigAddOptions)

// withAllowEphemeralPath opts a rig add out of the non-persistent-path refusal.
func withAllowEphemeralPath(allow bool) rigAddOption {
	return func(o *rigAddOptions) { o.allowEphemeralPath = allow }
}

// newRigAddOptions folds the supplied options over the defaults, which refuse a
// rig path that cannot survive a restart.
func newRigAddOptions(opts ...rigAddOption) rigAddOptions {
	var o rigAddOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
