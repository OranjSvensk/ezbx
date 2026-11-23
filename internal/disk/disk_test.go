package disk

import (
	"path/filepath"
	"testing"
)

func TestFree(t *testing.T) {
	dir := t.TempDir()
	n, err := Free(dir)
	if err != nil || n == 0 {
		t.Fatalf("Free(%s) = %d, %v", dir, n, err)
	}
	// A folder that doesn't exist yet is measured where it would be made.
	m, err := Free(filepath.Join(dir, "not", "made", "yet"))
	if err != nil || m == 0 {
		t.Fatalf("Free of a missing folder = %d, %v", m, err)
	}
}
