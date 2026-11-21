//go:build !linux

package workspace

// openChildDir opens the directory name inside parentFd with O_NOFOLLOW
// and refuses it when its st_dev differs from the parent's. macOS has no
// bind mounts; its mounts (volumes, disk images, network and FUSE
// filesystems) each have their own st_dev, which this catches. A
// same-device nullfs or similar mount on other BSDs is not detected.
func openChildDir(parentFd int, name string, _ bool) (int, error) {
	return openSameDevice(parentFd, name)
}

// mountCheckAvailable is always true: the st_dev check needs nothing
// beyond openat.
func mountCheckAvailable() bool { return true }
