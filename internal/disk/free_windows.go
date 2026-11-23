//go:build windows

package disk

import (
	"syscall"
	"unsafe"
)

// kernel32 is one of Windows' known DLLs, always loaded from the system
// folder, so this can't pick up a planted copy.
var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

func free(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, all uint64
	r, _, err := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&all)))
	if r == 0 {
		return 0, err
	}
	return avail, nil
}
