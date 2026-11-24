//go:build unix

package readonly

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A named pipe must never be opened by the scan: reading one waits for a
// writer forever.
func TestWalkSkipsPipes(t *testing.T) {
	root := tree(t, map[string]string{"a.txt": ""})
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skip("can't make a pipe:", err)
	}
	w, err := Walk(root, defaults(t))
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(w.Scan, ","); s != "a.txt" {
		t.Errorf("scan list %q", s)
	}
}
