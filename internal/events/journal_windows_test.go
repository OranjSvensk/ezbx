package events

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestGatewaySnapshotRetriesWindowsSharingViolation(t *testing.T) {
	j := OpenGateway(t.TempDir(), "vt")
	appendEvent(t, j, "gateway.request", 0)
	path, err := syscall.UTF16PtrFromString(j.file)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(path, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.read(); !errors.Is(err, syscall.Errno(32)) {
		syscall.CloseHandle(handle)
		t.Fatalf("expected a sharing violation with the exclusive handle open, got %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	release := time.AfterFunc(80*time.Millisecond, func() { syscall.CloseHandle(handle) })
	defer func() {
		if release.Stop() {
			syscall.CloseHandle(handle)
		}
	}()

	h, err := j.snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Entries) != 1 || h.Entries[0].Type != "gateway.request" {
		t.Fatalf("snapshot after the sharing violation: %+v", h)
	}
}
