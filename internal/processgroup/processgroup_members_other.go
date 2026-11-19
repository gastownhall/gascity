//go:build !windows && !darwin

package processgroup

import "errors"

// errMembersUnavailable reports that this platform has no member reader, so an
// EPERM from kill(-pgid, ...) keeps its plain meaning: a member we may not
// signal.
var errMembersUnavailable = errors.New("processgroup: group members are not readable on this platform")

func defaultLiveMembers(int) (int, error) {
	return 0, errMembersUnavailable
}

func defaultMemberStates(int) ([]MemberState, error) {
	return nil, errMembersUnavailable
}
