package safefs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// AvailableBytes returns the space available to non-root users on the
// filesystem containing path, using statfs f_bavail (not f_bfree).
func AvailableBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// CheckCapacity fails when the filesystem containing path has less than
// minFreeBytes available to the current (non-root) user.
func CheckCapacity(path string, minFreeBytes uint64) error {
	avail, err := AvailableBytes(path)
	if err != nil {
		return err
	}
	if avail < minFreeBytes {
		return fmt.Errorf("insufficient free space on %s: need at least %d bytes, have %d", path, minFreeBytes, avail)
	}
	return nil
}

// DeviceID returns the statfs filesystem identifier, used to verify that two
// directories share one filesystem.
func DeviceID(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	fsid := uint64(st.Fsid.Val[0])<<32 | uint64(uint32(st.Fsid.Val[1]))
	return fsid, nil
}
