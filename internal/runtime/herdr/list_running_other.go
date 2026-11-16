//go:build !windows

package herdr

func listRunningFailure(err error) error {
	return err
}
