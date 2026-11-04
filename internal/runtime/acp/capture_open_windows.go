//go:build windows

package acp

// captureNoFollow is zero on Windows, which has no O_NOFOLLOW. The transcript
// lives in a directory runtime.EnsurePrivateDir restricts to this user, so no
// other user can plant a link there.
const captureNoFollow = 0
