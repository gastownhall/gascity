package config

import "regexp"

// envVarName matches a POSIX-shaped environment variable name.
//
// Several config fields reference a credential by naming the environment
// variable that holds it rather than carrying the credential itself —
// `webhook_secret_env` on a GitHub monitor, `auth = "env:NAME"` on a storage
// binding. They share this shape so the set of names one accepts is the set
// the others accept, and so "is this a variable name or a pasted secret?" has
// exactly one answer in this package.
var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
