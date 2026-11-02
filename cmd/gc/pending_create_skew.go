package main

import "time"

// pendingCreateFutureSkew bounds how far in the future a pending create's
// lease anchor (last_woke_at, pending_create_started_at) may sit and still
// hold the row: a later one proves nothing (a clock stepped back), and must
// never pin a row.
const pendingCreateFutureSkew = 5 * time.Minute
