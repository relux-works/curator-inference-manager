//go:build !windows

package infra

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

func openOrCreateDirAt(parent int, name string) (int, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		if err = unix.Mkdirat(parent, name, 0o700); err != nil && !errors.Is(err, syscall.EEXIST) {
			return -1, err
		}
		fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, errors.New("managed state component is not a directory")
	}
	return fd, nil
}
