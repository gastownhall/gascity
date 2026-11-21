package main

import "time"

// prepareBudgetOpts is the one place the prepare-budget tests name the option.
func prepareBudgetOpts(d time.Duration) []startExecutionOption {
	return []startExecutionOption{withPrepareBudget(d)}
}
