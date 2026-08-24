//go:build unix

package instance

import (
	"errors"
	"os"
	"syscall"
)

var errLockHeld = errors.New("instance lock is held")

type platformLock struct{}

func tryPlatformLock(file *os.File) (*platformLock, error) {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errLockHeld
		}
		return nil, err
	}
	return &platformLock{}, nil
}

func unlockPlatform(file *os.File, _ *platformLock) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
