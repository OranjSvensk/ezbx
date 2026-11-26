package events

import (
	"os"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func tryLock(f *os.File) (bool, error) {
	const (
		failImmediately = 1
		exclusive       = 2
		lockViolation   = syscall.Errno(33)
	)
	var overlapped syscall.Overlapped
	ok, _, err := lockFileEx.Call(f.Fd(), failImmediately|exclusive, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if ok != 0 {
		return true, nil
	}
	if err == lockViolation {
		return false, nil
	}
	return false, err
}
