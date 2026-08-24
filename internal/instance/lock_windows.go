//go:build windows

package instance

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

var errLockHeld = errors.New("instance lock is held")

type platformLock struct {
	overlapped windows.Overlapped
}

func tryPlatformLock(file *os.File) (*platformLock, error) {
	lock := &platformLock{}
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&lock.overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil, errLockHeld
	}
	if err != nil {
		return nil, err
	}
	return lock, nil
}

func unlockPlatform(file *os.File, lock *platformLock) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &lock.overlapped)
}
