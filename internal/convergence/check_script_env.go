package convergence

import "github.com/gastownhall/gascity/internal/execenv"

// Operator-supplied check-script environment.
//
// ConditionEnv.Environ builds an explicit whitelist and sets HOME to the city
// directory, so a check script cannot discover configuration or credentials
// under the controller's home by accident. Some tools a check script runs
// locate their own inputs through the environment all the same: Python
// derives its per-user site-packages directory from HOME, so a module
// installed with `pip install --user` is invisible to a check even when the
// same interpreter finds it from the operator's shell. Rather than widen HOME,
// an operator can hand the check exactly the variables such a tool needs
// through [check_script_env] in city.toml. Those variables are appended to the
// whitelist; they can never replace a variable the whitelist itself sets.
//
// The name rules (which keys are reserved, what a valid name is) live in
// internal/execenv so the config loader can validate the table without
// importing this package; execenv.CheckScriptEnvWrittenKeys must list every
// key Environ writes, and TestEveryEnvironKeyIsReserved pins the two together.

// ValidateCheckScriptEnv rejects a [check_script_env] table whose keys are not
// valid environment variable names or are reserved.
func ValidateCheckScriptEnv(env map[string]string) error {
	return execenv.ValidateCheckScriptEnv(env)
}
